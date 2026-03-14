use std::collections::HashSet;
use std::path::PathBuf;

use anyhow::{bail, Context, Result};
use clap::{ArgAction, Parser};
use serde_json::{Map, Value};

#[derive(Debug, Clone)]
pub struct Config {
    pub telegram_bot_token: String,
    pub telegram_allowed_chat_ids: Option<HashSet<i64>>,
    pub telegram_poll_timeout_seconds: u64,
    pub telegram_message_chunk_size: usize,
    pub state_path: PathBuf,
    pub codex_cwd: PathBuf,
    pub codex_start_app_server: bool,
    pub codex_app_server_command: Vec<String>,
    pub codex_app_server_ws_url: Option<String>,
    pub codex_model: Option<String>,
    pub codex_personality: Option<String>,
    pub codex_sandbox: Option<String>,
    pub codex_approval_policy: Option<String>,
    pub codex_service_tier: Option<String>,
    pub codex_base_instructions: Option<String>,
    pub codex_developer_instructions: Option<String>,
    pub codex_config: Option<Map<String, Value>>,
    pub codex_ephemeral_threads: bool,
}

#[derive(Debug, Parser)]
#[command(name = "telegram-codex-relay")]
#[command(about = "Relay Telegram bot messages to the Codex app server.")]
struct RawCli {
    #[arg(long, env = "TELEGRAM_BOT_TOKEN")]
    telegram_bot_token: String,

    #[arg(long, env = "TELEGRAM_ALLOWED_CHAT_IDS")]
    telegram_allowed_chat_ids: Option<String>,

    #[arg(long, env = "TELEGRAM_POLL_TIMEOUT_SECONDS", default_value_t = 30)]
    telegram_poll_timeout_seconds: u64,

    #[arg(long, env = "TELEGRAM_MESSAGE_CHUNK_SIZE", default_value_t = 3900)]
    telegram_message_chunk_size: usize,

    #[arg(long, env = "RELAY_STATE_PATH", default_value = ".relay-state.json")]
    state_path: PathBuf,

    #[arg(long, env = "CODEX_CWD", default_value = ".")]
    codex_cwd: PathBuf,

    #[arg(long, action = ArgAction::SetTrue, conflicts_with = "no_start_app_server")]
    start_app_server: bool,

    #[arg(long, action = ArgAction::SetTrue, conflicts_with = "start_app_server")]
    no_start_app_server: bool,

    #[arg(long, env = "CODEX_START_APP_SERVER")]
    codex_start_app_server: Option<bool>,

    #[arg(long, env = "CODEX_APP_SERVER_COMMAND", default_value = "codex app-server")]
    codex_app_server_command: String,

    #[arg(long, env = "CODEX_APP_SERVER_WS_URL")]
    codex_app_server_ws_url: Option<String>,

    #[arg(long, env = "CODEX_MODEL")]
    codex_model: Option<String>,

    #[arg(long, env = "CODEX_PERSONALITY", default_value = "pragmatic")]
    codex_personality: Option<String>,

    #[arg(long, env = "CODEX_SANDBOX", default_value = "workspace-write")]
    codex_sandbox: Option<String>,

    #[arg(long, env = "CODEX_APPROVAL_POLICY", default_value = "never")]
    codex_approval_policy: Option<String>,

    #[arg(long, env = "CODEX_SERVICE_TIER")]
    codex_service_tier: Option<String>,

    #[arg(long, env = "CODEX_BASE_INSTRUCTIONS")]
    codex_base_instructions: Option<String>,

    #[arg(long, env = "CODEX_DEVELOPER_INSTRUCTIONS")]
    codex_developer_instructions: Option<String>,

    #[arg(long, env = "CODEX_CONFIG_JSON")]
    codex_config_json: Option<String>,

    #[arg(long, action = ArgAction::SetTrue, conflicts_with = "no_codex_ephemeral_threads")]
    codex_ephemeral_threads: bool,

    #[arg(long, action = ArgAction::SetTrue, conflicts_with = "codex_ephemeral_threads")]
    no_codex_ephemeral_threads: bool,

    #[arg(long, env = "CODEX_EPHEMERAL_THREADS")]
    codex_ephemeral_threads_env: Option<bool>,
}

impl Config {
    pub fn from_cli() -> Result<Self> {
        let raw = RawCli::parse();

        let codex_start_app_server = if raw.start_app_server {
            true
        } else if raw.no_start_app_server {
            false
        } else {
            raw.codex_start_app_server.unwrap_or(true)
        };

        if !codex_start_app_server && raw.codex_app_server_ws_url.is_none() {
            bail!("--no-start-app-server requires --codex-app-server-ws-url");
        }

        let codex_ephemeral_threads = if raw.codex_ephemeral_threads {
            true
        } else if raw.no_codex_ephemeral_threads {
            false
        } else {
            raw.codex_ephemeral_threads_env.unwrap_or(false)
        };

        let telegram_allowed_chat_ids = parse_allowed_chat_ids(raw.telegram_allowed_chat_ids)?;
        let codex_app_server_command = shell_words::split(&raw.codex_app_server_command)
            .context("failed to parse CODEX_APP_SERVER_COMMAND")?;
        if codex_app_server_command.is_empty() {
            bail!("CODEX_APP_SERVER_COMMAND cannot be empty");
        }

        let codex_config = parse_config_json(raw.codex_config_json)?;

        Ok(Self {
            telegram_bot_token: raw.telegram_bot_token,
            telegram_allowed_chat_ids,
            telegram_poll_timeout_seconds: raw.telegram_poll_timeout_seconds,
            telegram_message_chunk_size: raw.telegram_message_chunk_size,
            state_path: raw.state_path,
            codex_cwd: raw.codex_cwd.canonicalize().unwrap_or(raw.codex_cwd),
            codex_start_app_server,
            codex_app_server_command,
            codex_app_server_ws_url: raw.codex_app_server_ws_url,
            codex_model: raw.codex_model,
            codex_personality: raw.codex_personality,
            codex_sandbox: raw.codex_sandbox,
            codex_approval_policy: raw.codex_approval_policy,
            codex_service_tier: raw.codex_service_tier,
            codex_base_instructions: raw.codex_base_instructions,
            codex_developer_instructions: raw.codex_developer_instructions,
            codex_config,
            codex_ephemeral_threads,
        })
    }
}

fn parse_allowed_chat_ids(raw: Option<String>) -> Result<Option<HashSet<i64>>> {
    let Some(raw) = raw else {
        return Ok(None);
    };

    let mut values = HashSet::new();
    for part in raw.split(',') {
        let trimmed = part.trim();
        if trimmed.is_empty() {
            continue;
        }
        values.insert(trimmed.parse::<i64>().with_context(|| {
            format!("failed to parse chat id '{trimmed}' in TELEGRAM_ALLOWED_CHAT_IDS")
        })?);
    }

    Ok(Some(values))
}

fn parse_config_json(raw: Option<String>) -> Result<Option<Map<String, Value>>> {
    let Some(raw) = raw else {
        return Ok(None);
    };

    let value: Value = serde_json::from_str(&raw).context("failed to parse CODEX_CONFIG_JSON")?;
    match value {
        Value::Object(map) => Ok(Some(map)),
        _ => bail!("CODEX_CONFIG_JSON must decode to a JSON object"),
    }
}
