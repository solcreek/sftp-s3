//! sftp-s3 (Rust POC): an SFTP server whose filesystem is an S3 bucket.
//!
//! Each user is confined to s3://$S3_BUCKET/$S3_PREFIX<user>/.
//! Uploads stream straight into an S3 multipart upload; nothing touches disk.

mod s3fs;

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, bail};
use russh::keys::{Algorithm, HashAlg, PrivateKey};
use russh::server::{Auth, Msg, Server as _, Session};
use russh::{Channel, ChannelId};
use tracing::info;

use crate::s3fs::{S3Fs, SftpSession};

#[derive(Clone)]
struct Server {
    fs: Arc<S3Fs>,
    users: Arc<HashMap<String, String>>,
}

impl russh::server::Server for Server {
    type Handler = SshSession;
    fn new_client(&mut self, peer: Option<SocketAddr>) -> SshSession {
        SshSession { srv: self.clone(), user: None, peer, channels: HashMap::new() }
    }
}

struct SshSession {
    srv: Server,
    user: Option<String>,
    peer: Option<SocketAddr>,
    channels: HashMap<ChannelId, Channel<Msg>>,
}

fn ct_eq(a: &[u8], b: &[u8]) -> bool {
    a.len() == b.len() && a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

impl russh::server::Handler for SshSession {
    type Error = anyhow::Error;

    async fn auth_password(&mut self, user: &str, password: &str) -> Result<Auth, Self::Error> {
        match self.srv.users.get(user) {
            Some(want) if ct_eq(want.as_bytes(), password.as_bytes()) => {
                info!(user, peer = ?self.peer, "login");
                self.user = Some(user.to_string());
                Ok(Auth::Accept)
            }
            _ => Ok(Auth::reject()),
        }
    }

    async fn channel_open_session(
        &mut self,
        channel: Channel<Msg>,
        reply: russh::server::ChannelOpenHandle,
        _session: &mut Session,
    ) -> Result<(), Self::Error> {
        self.channels.insert(channel.id(), channel);
        reply.accept().await;
        Ok(())
    }

    async fn channel_eof(&mut self, channel: ChannelId, session: &mut Session) -> Result<(), Self::Error> {
        session.close(channel)?;
        Ok(())
    }

    async fn subsystem_request(
        &mut self,
        channel_id: ChannelId,
        name: &str,
        session: &mut Session,
    ) -> Result<(), Self::Error> {
        let (Some(user), true) = (self.user.clone(), name == "sftp") else {
            session.channel_failure(channel_id)?;
            return Ok(());
        };
        let Some(channel) = self.channels.remove(&channel_id) else {
            session.channel_failure(channel_id)?;
            return Ok(());
        };
        session.channel_success(channel_id)?;
        let sftp = SftpSession::new(self.srv.fs.clone(), &user);
        russh_sftp::server::run(channel.into_stream(), sftp).await; // spawns internally
        Ok(())
    }
}

/// SFTP_USERS="alice:secret,bob:hunter2"
fn parse_users(s: &str) -> anyhow::Result<HashMap<String, String>> {
    let m: HashMap<_, _> = s
        .split(',')
        .filter_map(|p| p.trim().split_once(':'))
        .filter(|(u, p)| !u.is_empty() && !p.is_empty() && !u.contains(['/', '.']))
        .map(|(u, p)| (u.to_string(), p.to_string()))
        .collect();
    if m.is_empty() {
        bail!("SFTP_USERS must be user:pass[,user:pass...]");
    }
    Ok(m)
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt().with_writer(std::io::stderr).with_ansi(false).with_env_filter(
        tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info,russh=warn".into()),
    ).init();

    let bucket = std::env::var("S3_BUCKET").context("S3_BUCKET is required")?;
    let users = parse_users(&std::env::var("SFTP_USERS").unwrap_or_default())?;

    let sdk = aws_config::load_defaults(aws_config::BehaviorVersion::latest()).await;
    let mut s3cfg = aws_sdk_s3::config::Builder::from(&sdk);
    if let Ok(ep) = std::env::var("S3_ENDPOINT") {
        s3cfg = s3cfg.endpoint_url(ep).force_path_style(true); // MinIO / R2 / etc.
    }
    let client = aws_sdk_s3::Client::from_conf(s3cfg.build());

    // Production must pin a key: an ephemeral one changes on every deploy and
    // clients will refuse to connect ("REMOTE HOST IDENTIFICATION HAS CHANGED").
    let key = match std::env::var("SFTP_HOST_KEY") {
        Ok(pem) => russh::keys::decode_secret_key(&pem, None)?,
        Err(_) => {
            tracing::warn!("SFTP_HOST_KEY not set; generating an ephemeral ed25519 host key");
            PrivateKey::random(&mut rand::rng(), Algorithm::Ed25519)?
        }
    };
    let fingerprint = key.public_key().fingerprint(HashAlg::Sha256).to_string();

    let config = russh::server::Config {
        auth_rejection_time: Duration::from_secs(1),
        auth_rejection_time_initial: Some(Duration::from_secs(0)),
        inactivity_timeout: Some(Duration::from_secs(600)),
        keys: vec![key],
        // Only offer password auth, so clients don't burn a rejection delay on
        // keyboard-interactive first.
        methods: russh::MethodSet::from(&[russh::MethodKind::Password][..]),
        ..Default::default()
    };

    let port: u16 = std::env::var("PORT").unwrap_or_else(|_| "2222".into()).parse()?;
    info!(port, bucket, fingerprint, "listening");
    let fs = Arc::new(S3Fs::new(client, bucket, std::env::var("S3_PREFIX").unwrap_or_default()));
    let mut server = Server { fs, users: Arc::new(users) };
    server.run_on_address(Arc::new(config), ("0.0.0.0", port)).await?;
    Ok(())
}
