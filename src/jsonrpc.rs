use std::collections::HashMap;
use std::process::Stdio;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use anyhow::{anyhow, bail, Context, Result};
use futures_util::{SinkExt, StreamExt};
use serde_json::{json, Value};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, Command};
use tokio::sync::{mpsc, oneshot, Mutex};
use tokio_tungstenite::connect_async;
use tokio_tungstenite::tungstenite::Message;
use tracing::{info, warn};

type PendingSender = oneshot::Sender<Result<Value>>;
type PendingMap = Arc<Mutex<HashMap<u64, PendingSender>>>;

pub struct JsonRpcClient {
    write_tx: mpsc::Sender<String>,
    notifications_rx: Mutex<mpsc::Receiver<Value>>,
    pending: PendingMap,
    next_id: AtomicU64,
    child: Option<Child>,
}

impl JsonRpcClient {
    pub async fn new_subprocess(command: &[String]) -> Result<Self> {
        let mut child = Command::new(&command[0])
            .args(&command[1..])
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .with_context(|| format!("failed to start '{}'", command.join(" ")))?;

        let stdin = child
            .stdin
            .take()
            .context("failed to capture Codex app-server stdin")?;
        let stdout = child
            .stdout
            .take()
            .context("failed to capture Codex app-server stdout")?;
        let stderr = child
            .stderr
            .take()
            .context("failed to capture Codex app-server stderr")?;

        let (write_tx, mut write_rx) = mpsc::channel::<String>(64);
        let (notifications_tx, notifications_rx) = mpsc::channel::<Value>(256);
        let pending: PendingMap = Arc::new(Mutex::new(HashMap::new()));

        tokio::spawn(async move {
            let mut stdin = stdin;
            while let Some(payload) = write_rx.recv().await {
                if stdin.write_all(payload.as_bytes()).await.is_err() {
                    break;
                }
                if stdin.write_all(b"\n").await.is_err() {
                    break;
                }
                if stdin.flush().await.is_err() {
                    break;
                }
            }
        });

        tokio::spawn(read_json_lines(stdout, notifications_tx.clone(), pending.clone()));
        tokio::spawn(log_stderr(stderr));

        Ok(Self {
            write_tx,
            notifications_rx: Mutex::new(notifications_rx),
            pending,
            next_id: AtomicU64::new(0),
            child: Some(child),
        })
    }

    pub async fn new_websocket(url: &str) -> Result<Self> {
        let (stream, _) = connect_async(url)
            .await
            .with_context(|| format!("failed to connect to app-server websocket at {url}"))?;
        let (mut writer, mut reader) = stream.split();

        let (write_tx, mut write_rx) = mpsc::channel::<String>(64);
        let (notifications_tx, notifications_rx) = mpsc::channel::<Value>(256);
        let pending: PendingMap = Arc::new(Mutex::new(HashMap::new()));

        tokio::spawn(async move {
            while let Some(payload) = write_rx.recv().await {
                if writer.send(Message::Text(payload.into())).await.is_err() {
                    break;
                }
            }
        });

        tokio::spawn(async move {
            while let Some(message) = reader.next().await {
                match message {
                    Ok(Message::Text(text)) => {
                        if let Err(error) =
                            route_incoming(&text, &notifications_tx, pending.clone()).await
                        {
                            warn!("failed to process websocket JSON-RPC message: {error:#}");
                        }
                    }
                    Ok(Message::Binary(bytes)) => {
                        match String::from_utf8(bytes.to_vec()) {
                            Ok(text) => {
                                if let Err(error) =
                                    route_incoming(&text, &notifications_tx, pending.clone()).await
                                {
                                    warn!(
                                        "failed to process websocket JSON-RPC binary message: {error:#}"
                                    );
                                }
                            }
                            Err(error) => warn!("invalid UTF-8 in websocket binary frame: {error}"),
                        }
                    }
                    Ok(Message::Close(_)) => break,
                    Ok(_) => {}
                    Err(error) => {
                        warn!("websocket reader failed: {error}");
                        break;
                    }
                }
            }
            fail_pending(&pending, anyhow!("Codex app-server websocket closed")).await;
        });

        Ok(Self {
            write_tx,
            notifications_rx: Mutex::new(notifications_rx),
            pending,
            next_id: AtomicU64::new(0),
            child: None,
        })
    }

    pub async fn request(&self, method: &str, params: Value) -> Result<Value> {
        let request_id = self.next_id.fetch_add(1, Ordering::SeqCst) + 1;
        let (tx, rx) = oneshot::channel();
        self.pending.lock().await.insert(request_id, tx);

        let payload = json!({
            "jsonrpc": "2.0",
            "id": request_id,
            "method": method,
            "params": params,
        });
        self.write_tx
            .send(payload.to_string())
            .await
            .context("failed to send JSON-RPC request")?;

        rx.await
            .context("JSON-RPC response channel closed before reply")?
    }

    pub async fn notify(&self, method: &str, params: Option<Value>) -> Result<()> {
        let payload = match params {
            Some(params) => json!({
                "jsonrpc": "2.0",
                "method": method,
                "params": params,
            }),
            None => json!({
                "jsonrpc": "2.0",
                "method": method,
            }),
        };
        self.write_tx
            .send(payload.to_string())
            .await
            .context("failed to send JSON-RPC notification")?;
        Ok(())
    }

    pub async fn next_notification(&self) -> Option<Value> {
        self.notifications_rx.lock().await.recv().await
    }

    pub async fn shutdown(&mut self) -> Result<()> {
        if let Some(child) = self.child.as_mut() {
            let _ = child.kill().await;
            let _ = child.wait().await;
        }
        Ok(())
    }
}

async fn read_json_lines<R>(
    reader: R,
    notifications_tx: mpsc::Sender<Value>,
    pending: PendingMap,
) where
    R: tokio::io::AsyncRead + Unpin + Send + 'static,
{
    let mut lines = BufReader::new(reader).lines();
    loop {
        match lines.next_line().await {
            Ok(Some(line)) => {
                if line.trim().is_empty() {
                    continue;
                }
                if let Err(error) = route_incoming(&line, &notifications_tx, pending.clone()).await
                {
                    warn!("failed to process JSON-RPC line: {error:#}");
                }
            }
            Ok(None) => break,
            Err(error) => {
                warn!("Codex app-server stdout reader failed: {error}");
                break;
            }
        }
    }
    fail_pending(&pending, anyhow!("Codex app-server stream closed")).await;
}

async fn log_stderr<R>(reader: R)
where
    R: tokio::io::AsyncRead + Unpin + Send + 'static,
{
    let mut lines = BufReader::new(reader).lines();
    while let Ok(Some(line)) = lines.next_line().await {
        info!("codex-app-server: {line}");
    }
}

async fn route_incoming(
    raw_message: &str,
    notifications_tx: &mpsc::Sender<Value>,
    pending: PendingMap,
) -> Result<()> {
    let value: Value = serde_json::from_str(raw_message).context("invalid JSON")?;
    let object = value
        .as_object()
        .cloned()
        .ok_or_else(|| anyhow!("JSON-RPC payload was not an object"))?;

    if let Some(id) = object.get("id").and_then(Value::as_u64) {
        if let Some(sender) = pending.lock().await.remove(&id) {
            if let Some(error) = object.get("error") {
                let message = error
                    .get("message")
                    .and_then(Value::as_str)
                    .unwrap_or("unknown JSON-RPC error");
                let _ = sender.send(Err(anyhow!(message.to_string())));
            } else {
                let result = object.get("result").cloned().unwrap_or(Value::Null);
                let _ = sender.send(Ok(result));
            }
        }
        return Ok(());
    }

    if object.get("method").is_some() {
        notifications_tx
            .send(Value::Object(object))
            .await
            .context("failed to queue JSON-RPC notification")?;
        return Ok(());
    }

    bail!("received malformed JSON-RPC message");
}

async fn fail_pending(pending: &PendingMap, error: anyhow::Error) {
    let mut pending = pending.lock().await;
    for (_, sender) in pending.drain() {
        let _ = sender.send(Err(anyhow!(error.to_string())));
    }
}
