mod app;
mod codex;
mod config;
mod jsonrpc;
mod telegram;

use anyhow::Result;
use app::RelayApp;
use config::Config;
use tracing_subscriber::EnvFilter;

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| EnvFilter::new("telegram_codex_relay=info")),
        )
        .init();

    let config = Config::from_cli()?;
    let mut app = RelayApp::new(config).await?;
    app.run().await
}
