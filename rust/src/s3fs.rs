//! SFTP handler backed by S3.
//!
//! russh-sftp processes one request at a time per session, so a write that
//! awaited an S3 part upload would stall the whole connection. Instead, full
//! parts are handed to background tasks (bounded by a semaphore, which is the
//! back-pressure), and only CLOSE waits for them and completes the upload.

use std::collections::{BTreeMap, HashMap};
use std::sync::Arc;
use std::time::Instant;

use aws_sdk_s3::Client;
use aws_sdk_s3::error::SdkError;
use aws_sdk_s3::primitives::ByteStream;
use aws_sdk_s3::types::{CompletedMultipartUpload, CompletedPart};
use russh_sftp::protocol::{
    Attrs, Data, File, FileAttributes, Handle, Name, OpenFlags, Status, StatusCode,
};
use russh_sftp::server::StatusReply;
use tokio::sync::Semaphore;
use tokio::task::JoinSet;
use tracing::{info, warn};

const PART_SIZE: usize = 8 << 20;
const PART_CONCURRENCY: usize = 4;
const MAX_REORDER: usize = 64 << 20;
const READ_WINDOW: u64 = 4 << 20;

pub struct S3Fs {
    client: Client,
    bucket: String,
    prefix: String,
}

impl S3Fs {
    pub fn new(client: Client, bucket: String, prefix: String) -> Self {
        Self { client, bucket, prefix }
    }
}

type R<T> = Result<T, StatusReply>;

fn fail(msg: impl std::fmt::Display) -> StatusReply {
    StatusCode::Failure.with_message(msg.to_string())
}

/// Map an SDK error: 404 → NoSuchFile, anything else → Failure with the message.
fn s3err<E, Rsp>(e: SdkError<E, Rsp>) -> StatusReply
where
    E: std::error::Error + 'static,
    Rsp: std::fmt::Debug,
    SdkError<E, Rsp>: std::error::Error,
{
    let is_404 = match &e {
        SdkError::ServiceError(se) => format!("{:?}", se.raw()).contains("status: 404")
            || format!("{:?}", se.err()).contains("NotFound")
            || format!("{:?}", se.err()).contains("NoSuchKey"),
        _ => false,
    };
    if is_404 {
        StatusCode::NoSuchFile.into()
    } else {
        fail(aws_sdk_s3::error::DisplayErrorContext(&e))
    }
}

fn ok(id: u32) -> Status {
    Status { id, status_code: StatusCode::Ok, error_message: "Ok".into(), language_tag: "en-US".into() }
}

fn dir_attrs() -> FileAttributes {
    let mut a = FileAttributes::default();
    a.permissions = Some(0o040755);
    a
}

fn file_attrs(size: u64, mtime: Option<i64>) -> FileAttributes {
    let mut a = FileAttributes::default();
    a.size = Some(size);
    a.permissions = Some(0o100644);
    a.mtime = mtime.map(|t| t as u32);
    a.atime = a.mtime;
    a
}

/// Normalize an SFTP path to "/a/b" form, resolving "." and "..".
fn clean(p: &str) -> String {
    let mut out: Vec<&str> = Vec::new();
    for seg in p.split('/') {
        match seg {
            "" | "." => {}
            ".." => {
                out.pop();
            }
            s => out.push(s),
        }
    }
    format!("/{}", out.join("/"))
}

fn base(p: &str) -> String {
    clean(p).rsplit('/').next().unwrap_or("").to_string()
}

struct Upload {
    key: String,
    upload_id: Option<String>, // created lazily: small files use a single PutObject
    buf: Vec<u8>,
    next_part: i32,
    tasks: JoinSet<Result<CompletedPart, String>>,
    done_parts: Vec<CompletedPart>,
    expected_off: u64,
    pending: BTreeMap<u64, Vec<u8>>,
    pending_bytes: usize,
    failed: Option<String>,
    started: Instant,
}

struct Download {
    key: String,
    win_off: u64,
    win: Vec<u8>,
}

enum Open {
    Write(Upload),
    Read(Download),
    Dir { path: String, done: bool },
}

pub struct SftpSession {
    fs: Arc<S3Fs>,
    root: String,
    handles: HashMap<String, Open>,
    next_handle: u64,
    sem: Arc<Semaphore>,
}

impl SftpSession {
    pub fn new(fs: Arc<S3Fs>, user: &str) -> Self {
        let root = format!("{}{}/", fs.prefix, user);
        Self { fs, root, handles: HashMap::new(), next_handle: 0, sem: Arc::new(Semaphore::new(PART_CONCURRENCY)) }
    }

    fn key(&self, p: &str) -> String {
        format!("{}{}", self.root, clean(p).trim_start_matches('/'))
    }

    fn dir_key(&self, p: &str) -> String {
        let k = self.key(p);
        if k.ends_with('/') { k } else { k + "/" }
    }

    fn new_handle(&mut self, o: Open) -> String {
        self.next_handle += 1;
        let h = self.next_handle.to_string();
        self.handles.insert(h.clone(), o);
        h
    }

    async fn stat_path(&self, path: &str) -> R<FileAttributes> {
        if clean(path) == "/" {
            return Ok(dir_attrs());
        }
        let c = &self.fs.client;
        let key = self.key(path);
        if let Ok(h) = c.head_object().bucket(&self.fs.bucket).key(&key).send().await {
            return Ok(file_attrs(h.content_length().unwrap_or(0) as u64, h.last_modified().map(|t| t.secs())));
        }
        let out = c.list_objects_v2().bucket(&self.fs.bucket).prefix(self.dir_key(path)).max_keys(1)
            .send().await.map_err(s3err)?;
        if out.key_count().unwrap_or(0) > 0 {
            Ok(dir_attrs())
        } else {
            Err(StatusCode::NoSuchFile.into())
        }
    }

    /// Append in-order bytes; ship every full part to a background task.
    async fn push(&self, up: &mut Upload, data: Vec<u8>) -> Result<(), String> {
        up.expected_off += data.len() as u64;
        if up.buf.is_empty() && data.len() >= PART_SIZE {
            up.buf = data;
        } else {
            up.buf.extend_from_slice(&data);
        }
        while up.buf.len() >= PART_SIZE {
            let rest = up.buf.split_off(PART_SIZE);
            let part = std::mem::replace(&mut up.buf, rest);
            self.ship(up, part).await?;
        }
        Ok(())
    }

    async fn ship(&self, up: &mut Upload, part: Vec<u8>) -> Result<(), String> {
        let c = self.fs.client.clone();
        let bucket = self.fs.bucket.clone();
        if up.upload_id.is_none() {
            let r = c.create_multipart_upload().bucket(&bucket).key(&up.key).send().await
                .map_err(|e| aws_sdk_s3::error::DisplayErrorContext(e).to_string())?;
            up.upload_id = r.upload_id().map(str::to_string);
        }
        // Collect finished parts; surface a failed part before sending more.
        while let Some(r) = up.tasks.try_join_next() {
            up.done_parts.push(r.map_err(|e| e.to_string())??);
        }
        // Back-pressure: at most PART_CONCURRENCY parts in flight per session.
        let permit = self.sem.clone().acquire_owned().await.map_err(|e| e.to_string())?;
        up.next_part += 1;
        let (n, key, id) = (up.next_part, up.key.clone(), up.upload_id.clone().unwrap_or_default());
        up.tasks.spawn(async move {
            let _permit = permit;
            let r = c.upload_part().bucket(bucket).key(key).upload_id(id).part_number(n)
                .body(ByteStream::from(part)).send().await
                .map_err(|e| aws_sdk_s3::error::DisplayErrorContext(e).to_string())?;
            Ok(CompletedPart::builder().part_number(n).set_e_tag(r.e_tag().map(str::to_string)).build())
        });
        Ok(())
    }

    async fn finish(&self, mut up: Upload) -> Result<u64, String> {
        let c = &self.fs.client;
        let bucket = &self.fs.bucket;
        if let Some(e) = up.failed.take() {
            self.abort(&up).await;
            return Err(e);
        }
        if !up.pending.is_empty() {
            self.abort(&up).await;
            return Err(format!("file closed with a hole at offset {}", up.expected_off));
        }
        let size = up.expected_off;
        if up.upload_id.is_none() {
            // Whole file fit in one part: single PutObject.
            c.put_object().bucket(bucket).key(&up.key).body(ByteStream::from(std::mem::take(&mut up.buf)))
                .send().await.map_err(|e| aws_sdk_s3::error::DisplayErrorContext(e).to_string())?;
            return Ok(size);
        }
        if !up.buf.is_empty() {
            let last = std::mem::take(&mut up.buf);
            if let Err(e) = self.ship(&mut up, last).await {
                self.abort(&up).await;
                return Err(e);
            }
        }
        let mut parts = std::mem::take(&mut up.done_parts);
        while let Some(r) = up.tasks.join_next().await {
            match r {
                Ok(Ok(p)) => parts.push(p),
                Ok(Err(e)) => {
                    self.abort(&up).await;
                    return Err(e);
                }
                Err(e) => {
                    self.abort(&up).await;
                    return Err(e.to_string());
                }
            }
        }
        parts.sort_by_key(|p| p.part_number());
        c.complete_multipart_upload().bucket(bucket).key(&up.key)
            .upload_id(up.upload_id.as_deref().unwrap_or_default())
            .multipart_upload(CompletedMultipartUpload::builder().set_parts(Some(parts)).build())
            .send().await.map_err(|e| aws_sdk_s3::error::DisplayErrorContext(e).to_string())?;
        Ok(size)
    }

    async fn abort(&self, up: &Upload) {
        if let Some(id) = &up.upload_id {
            let _ = self.fs.client.abort_multipart_upload().bucket(&self.fs.bucket).key(&up.key)
                .upload_id(id).send().await;
        }
    }
}

impl Drop for SftpSession {
    /// Client vanished mid-upload: abort dangling multipart uploads so they do
    /// not accrue storage. (Also set an AbortIncompleteMultipartUpload
    /// lifecycle rule on the bucket as a backstop.)
    fn drop(&mut self) {
        for (_, o) in self.handles.drain() {
            if let Open::Write(up) = o {
                if let Some(id) = up.upload_id {
                    warn!(key = up.key, "aborting unfinished upload");
                    let (c, b) = (self.fs.client.clone(), self.fs.bucket.clone());
                    tokio::spawn(async move {
                        let _ = c.abort_multipart_upload().bucket(b).key(up.key).upload_id(id).send().await;
                    });
                }
            }
        }
    }
}

impl russh_sftp::server::Handler for SftpSession {
    type Error = StatusReply;

    fn unimplemented(&self) -> Self::Error {
        StatusCode::OpUnsupported.into()
    }

    async fn realpath(&mut self, id: u32, path: String) -> R<Name> {
        Ok(Name { id, files: vec![File::dummy(clean(&path))] })
    }

    async fn stat(&mut self, id: u32, path: String) -> R<Attrs> {
        Ok(Attrs { id, attrs: self.stat_path(&path).await? })
    }

    async fn lstat(&mut self, id: u32, path: String) -> R<Attrs> {
        self.stat(id, path).await
    }

    async fn fstat(&mut self, id: u32, handle: String) -> R<Attrs> {
        match self.handles.get(&handle) {
            Some(Open::Write(up)) => Ok(Attrs { id, attrs: file_attrs(up.expected_off, None) }),
            Some(Open::Read(d)) => {
                let k = d.key.trim_start_matches(&self.root).to_string();
                Ok(Attrs { id, attrs: self.stat_path(&k).await? })
            }
            Some(Open::Dir { .. }) => Ok(Attrs { id, attrs: dir_attrs() }),
            None => Err(fail("bad handle")),
        }
    }

    async fn setstat(&mut self, id: u32, _path: String, _attrs: FileAttributes) -> R<Status> {
        Ok(ok(id)) // chmod/chown/utimes have no S3 equivalent; accept and ignore
    }

    async fn fsetstat(&mut self, id: u32, _handle: String, _attrs: FileAttributes) -> R<Status> {
        Ok(ok(id))
    }

    async fn open(&mut self, id: u32, filename: String, pflags: OpenFlags, _attrs: FileAttributes) -> R<Handle> {
        let key = self.key(&filename);
        let o = if pflags.contains(OpenFlags::WRITE) {
            info!(key, "upload start");
            Open::Write(Upload {
                key, upload_id: None, buf: Vec::new(), next_part: 0, tasks: JoinSet::new(), done_parts: Vec::new(),
                expected_off: 0, pending: BTreeMap::new(), pending_bytes: 0, failed: None,
                started: Instant::now(),
            })
        } else {
            self.fs.client.head_object().bucket(&self.fs.bucket).key(&key).send().await.map_err(s3err)?;
            Open::Read(Download { key, win_off: 0, win: Vec::new() })
        };
        Ok(Handle { id, handle: self.new_handle(o) })
    }

    async fn write(&mut self, id: u32, handle: String, offset: u64, data: Vec<u8>) -> R<Status> {
        let Some(Open::Write(mut up)) = self.handles.remove(&handle) else {
            return Err(fail("bad handle"));
        };
        let res: Result<(), String> = async {
            if let Some(e) = &up.failed {
                return Err(e.clone());
            }
            if offset < up.expected_off {
                return Err(format!("random-access write at {offset} (stream at {}) is not supported", up.expected_off));
            }
            if offset > up.expected_off {
                // russh-sftp serializes requests, so this only happens if the client itself
                // sends out of order. Park it, bounded.
                if up.pending_bytes + data.len() > MAX_REORDER {
                    return Err(format!("out-of-order window exceeded {MAX_REORDER} bytes"));
                }
                up.pending_bytes += data.len();
                up.pending.insert(offset, data);
                return Ok(());
            }
            self.push(&mut up, data).await?;
            while let Some(next) = up.pending.remove(&up.expected_off) {
                up.pending_bytes -= next.len();
                self.push(&mut up, next).await?;
            }
            Ok(())
        }.await;
        if let Err(e) = &res {
            up.failed.get_or_insert(e.clone());
        }
        self.handles.insert(handle, Open::Write(up));
        res.map(|_| ok(id)).map_err(fail)
    }

    async fn read(&mut self, id: u32, handle: String, offset: u64, len: u32) -> R<Data> {
        let (c, bucket) = (self.fs.client.clone(), self.fs.bucket.clone());
        let Some(Open::Read(d)) = self.handles.get_mut(&handle) else {
            return Err(fail("bad handle"));
        };
        let end = offset + len as u64;
        if offset < d.win_off || end > d.win_off + d.win.len() as u64 {
            let range = format!("bytes={}-{}", offset, offset + READ_WINDOW - 1);
            let out = match c.get_object().bucket(bucket).key(&d.key).range(range).send().await {
                Ok(o) => o,
                Err(e) if e.raw_response().map(|r| r.status().as_u16()) == Some(416) => {
                    return Err(StatusCode::Eof.into());
                }
                Err(e) => return Err(s3err(e)),
            };
            d.win = out.body.collect().await.map_err(fail)?.into_bytes().to_vec();
            d.win_off = offset;
        }
        let start = (offset - d.win_off) as usize;
        if start >= d.win.len() {
            return Err(StatusCode::Eof.into());
        }
        let stop = (start + len as usize).min(d.win.len());
        Ok(Data { id, data: d.win[start..stop].to_vec() })
    }

    async fn close(&mut self, id: u32, handle: String) -> R<Status> {
        match self.handles.remove(&handle) {
            Some(Open::Write(up)) => {
                let (key, started) = (up.key.clone(), up.started);
                // The upload result is the CLOSE status: a failed upload is never reported "ok".
                let r = self.finish(up).await;
                let secs = started.elapsed().as_secs_f64();
                match r {
                    Ok(bytes) => {
                        info!(key, bytes, secs = format!("{secs:.2}"),
                              MBps = format!("{:.1}", bytes as f64 / 1e6 / secs.max(1e-9)), "upload done");
                        Ok(ok(id))
                    }
                    Err(e) => {
                        warn!(key, err = e, "upload failed");
                        Err(fail(e))
                    }
                }
            }
            Some(_) => Ok(ok(id)),
            None => Err(fail("bad handle")),
        }
    }

    async fn opendir(&mut self, id: u32, path: String) -> R<Handle> {
        let attrs = self.stat_path(&path).await?;
        if attrs.permissions.unwrap_or(0) & 0o040000 == 0 {
            return Err(StatusCode::NoSuchFile.with_message("not a directory"));
        }
        Ok(Handle { id, handle: self.new_handle(Open::Dir { path, done: false }) })
    }

    async fn readdir(&mut self, id: u32, handle: String) -> R<Name> {
        let Some(Open::Dir { path, done }) = self.handles.get_mut(&handle) else {
            return Err(fail("bad handle"));
        };
        if *done {
            return Err(StatusCode::Eof.into());
        }
        *done = true;
        let path = path.clone();
        let prefix = if clean(&path) == "/" { self.root.clone() } else { self.dir_key(&path) };
        let mut files = Vec::new();
        let mut pages = self.fs.client.list_objects_v2().bucket(&self.fs.bucket).prefix(&prefix)
            .delimiter("/").into_paginator().send();
        while let Some(page) = pages.next().await {
            let page = page.map_err(s3err)?;
            for cp in page.common_prefixes() {
                let p = cp.prefix().unwrap_or_default().trim_end_matches('/');
                files.push(File::new(base(p), dir_attrs()));
            }
            for o in page.contents() {
                let k = o.key().unwrap_or_default();
                if k == prefix {
                    continue; // the directory marker itself
                }
                files.push(File::new(base(k), file_attrs(o.size().unwrap_or(0) as u64, o.last_modified().map(|t| t.secs()))));
            }
        }
        if files.is_empty() {
            return Err(StatusCode::Eof.into());
        }
        Ok(Name { id, files })
    }

    async fn mkdir(&mut self, id: u32, path: String, _attrs: FileAttributes) -> R<Status> {
        // zero-byte "dir/" marker so empty directories show up in listings
        self.fs.client.put_object().bucket(&self.fs.bucket).key(self.dir_key(&path))
            .body(ByteStream::from_static(b"")).send().await.map_err(s3err)?;
        Ok(ok(id))
    }

    async fn rmdir(&mut self, id: u32, path: String) -> R<Status> {
        let dk = self.dir_key(&path);
        let out = self.fs.client.list_objects_v2().bucket(&self.fs.bucket).prefix(&dk).max_keys(2)
            .send().await.map_err(s3err)?;
        if out.contents().iter().any(|o| o.key() != Some(dk.as_str())) {
            return Err(fail("directory not empty"));
        }
        self.fs.client.delete_object().bucket(&self.fs.bucket).key(dk).send().await.map_err(s3err)?;
        Ok(ok(id))
    }

    async fn remove(&mut self, id: u32, filename: String) -> R<Status> {
        self.fs.client.delete_object().bucket(&self.fs.bucket).key(self.key(&filename))
            .send().await.map_err(s3err)?;
        Ok(ok(id))
    }

    async fn rename(&mut self, id: u32, oldpath: String, newpath: String) -> R<Status> {
        // WinSCP/FileZilla upload to "x.filepart" then rename; S3 needs copy+delete.
        // CopyObject handles objects up to 5 GB, enough for a POC.
        let (src, dst) = (self.key(&oldpath), self.key(&newpath));
        let c = &self.fs.client;
        c.copy_object().bucket(&self.fs.bucket).key(&dst).copy_source(format!("{}/{}", self.fs.bucket, src))
            .send().await.map_err(s3err)?;
        c.delete_object().bucket(&self.fs.bucket).key(&src).send().await.map_err(s3err)?;
        Ok(ok(id))
    }
}
