use std::collections::HashMap;

use anyhow::{Context, Result};
use tokio::fs;
use tracing::warn;

use crate::codex::CodexClient;
use crate::config::Config;
use crate::telegram::{chunk_message, TelegramClient};

pub struct RelayApp {
    config: Config,
    telegram: TelegramClient,
    codex: CodexClient,
    thread_ids_by_chat: HashMap<String, String>,
}

impl RelayApp {
    pub async fn new(config: Config) -> Result<Self> {
        let telegram = TelegramClient::new(&config.telegram_bot_token)?;
        let codex = CodexClient::connect(config.clone()).await?;
        Ok(Self {
            config,
            telegram,
            codex,
            thread_ids_by_chat: HashMap::new(),
        })
    }

    pub async fn run(&mut self) -> Result<()> {
        self.load_state().await?;
        self.telegram.delete_webhook(false).await?;

        let mut offset = None;
        loop {
            let updates = self
                .telegram
                .get_updates(offset, self.config.telegram_poll_timeout_seconds)
                .await?;

            for update in updates {
                offset = Some(update.update_id + 1);
                self.handle_update(update).await?;
            }
        }
    }

    async fn handle_update(&mut self, update: crate::telegram::Update) -> Result<()> {
        let Some(message) = update.message else {
            return Ok(());
        };

        if !self.is_chat_allowed(message.chat.id) {
            warn!("ignoring message from unauthorized chat {}", message.chat.id);
            return Ok(());
        }

        let Some(text) = message.text.as_deref() else {
            self.telegram
                .send_message(
                    message.chat.id,
                    "Only plain text messages are supported right now.",
                    Some(message.message_id),
                )
                .await?;
            return Ok(());
        };

        if text.starts_with('/') {
            self.handle_command(message.chat.id, message.message_id, text)
                .await?;
            return Ok(());
        }

        self.relay_message(message.chat.id, message.message_id, text)
            .await
    }

    async fn handle_command(
        &mut self,
        chat_id: i64,
        message_id: i64,
        text: &str,
    ) -> Result<()> {
        let command = text.split_whitespace().next().unwrap_or_default();
        let command = command.split('@').next().unwrap_or(command);

        match command {
            "/new" | "/reset" => {
                let thread_id = self.codex.new_thread().await?;
                self.thread_ids_by_chat
                    .insert(chat_id.to_string(), thread_id.clone());
                self.save_state().await?;
                self.telegram
                    .send_message(
                        chat_id,
                        &format!("Started a new Codex thread.\nthread_id={thread_id}"),
                        Some(message_id),
                    )
                    .await?;
            }
            "/status" => {
                let current_thread = self
                    .thread_ids_by_chat
                    .get(&chat_id.to_string())
                    .cloned()
                    .unwrap_or_else(|| "(not started yet)".to_string());
                let transport = if self.config.codex_start_app_server {
                    "stdio subprocess"
                } else {
                    "websocket"
                };
                let status = format!(
                    "Transport: {transport}\nThread: {current_thread}\nCWD: {}",
                    self.config.codex_cwd.display()
                );
                self.telegram
                    .send_message(chat_id, &status, Some(message_id))
                    .await?;
            }
            "/help" => {
                let help = "Send any text message to relay it to Codex.\n/new or /reset starts a fresh Codex thread.\n/status shows the current thread mapping.";
                self.telegram
                    .send_message(chat_id, help, Some(message_id))
                    .await?;
            }
            _ => {
                self.telegram
                    .send_message(
                        chat_id,
                        "Unknown command. Use /help for the supported commands.",
                        Some(message_id),
                    )
                    .await?;
            }
        }

        Ok(())
    }

    async fn relay_message(&mut self, chat_id: i64, message_id: i64, text: &str) -> Result<()> {
        let current_thread_id = self.thread_ids_by_chat.get(&chat_id.to_string()).cloned();
        let thread_id = self.codex.ensure_thread(current_thread_id.as_deref()).await?;
        if current_thread_id.as_deref() != Some(thread_id.as_str()) {
            self.thread_ids_by_chat
                .insert(chat_id.to_string(), thread_id.clone());
            self.save_state().await?;
        }

        let result = self.codex.run_turn(&thread_id, text).await?;
        let reply_text = match (result.error_message, result.text.trim()) {
            (Some(error), "") => format!("Codex reported an error: {error}"),
            (Some(error), text) => format!("Codex reported an error: {error}\n\n{text}"),
            (None, "") => "Codex completed the turn without returning assistant text.".to_string(),
            (None, text) => text.to_string(),
        };

        for (index, chunk) in chunk_message(&reply_text, self.config.telegram_message_chunk_size)
            .into_iter()
            .enumerate()
        {
            self.telegram
                .send_message(
                    chat_id,
                    &chunk,
                    if index == 0 { Some(message_id) } else { None },
                )
                .await?;
        }

        Ok(())
    }

    fn is_chat_allowed(&self, chat_id: i64) -> bool {
        self.config
            .telegram_allowed_chat_ids
            .as_ref()
            .map(|allowed| allowed.contains(&chat_id))
            .unwrap_or(true)
    }

    async fn load_state(&mut self) -> Result<()> {
        let Ok(raw) = fs::read_to_string(&self.config.state_path).await else {
            self.thread_ids_by_chat.clear();
            return Ok(());
        };

        match serde_json::from_str::<HashMap<String, String>>(&raw) {
            Ok(mapping) => self.thread_ids_by_chat = mapping,
            Err(error) => {
                warn!(
                    "state file '{}' is not valid JSON: {error}",
                    self.config.state_path.display()
                );
                self.thread_ids_by_chat.clear();
            }
        }
        Ok(())
    }

    async fn save_state(&self) -> Result<()> {
        if let Some(parent) = self.config.state_path.parent() {
            fs::create_dir_all(parent).await.with_context(|| {
                format!("failed to create state directory '{}'", parent.display())
            })?;
        }

        let payload = serde_json::to_string_pretty(&self.thread_ids_by_chat)
            .context("failed to serialize relay state")?;
        fs::write(&self.config.state_path, format!("{payload}\n"))
            .await
            .with_context(|| {
                format!(
                    "failed to write relay state '{}'",
                    self.config.state_path.display()
                )
            })
    }
}
