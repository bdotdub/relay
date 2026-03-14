use std::collections::HashSet;

use anyhow::{Context, Result};
use serde_json::{json, Map, Value};

use crate::config::Config;
use crate::jsonrpc::JsonRpcClient;

pub struct CodexClient {
    rpc: JsonRpcClient,
    config: Config,
    loaded_threads: HashSet<String>,
}

#[derive(Debug, Clone)]
pub struct TurnResult {
    pub text: String,
    pub error_message: Option<String>,
}

impl CodexClient {
    pub async fn connect(config: Config) -> Result<Self> {
        let rpc = if config.codex_start_app_server {
            JsonRpcClient::new_subprocess(&config.codex_app_server_command).await?
        } else {
            JsonRpcClient::new_websocket(
                config
                    .codex_app_server_ws_url
                    .as_deref()
                    .context("missing websocket URL for external app-server mode")?,
            )
            .await?
        };

        let client = Self {
            rpc,
            config,
            loaded_threads: HashSet::new(),
        };
        client.initialize().await?;
        Ok(client)
    }

    pub async fn shutdown(&mut self) -> Result<()> {
        self.rpc.shutdown().await
    }

    pub async fn new_thread(&mut self) -> Result<String> {
        let response = self
            .rpc
            .request("thread/start", Value::Object(self.new_thread_params()))
            .await?;
        let thread_id = response
            .get("thread")
            .and_then(Value::as_object)
            .and_then(|thread| thread.get("id"))
            .and_then(Value::as_str)
            .context("thread/start response missing thread.id")?
            .to_string();
        self.loaded_threads.insert(thread_id.clone());
        Ok(thread_id)
    }

    pub async fn ensure_thread(&mut self, thread_id: Option<&str>) -> Result<String> {
        let Some(thread_id) = thread_id else {
            return self.new_thread().await;
        };

        if self.loaded_threads.contains(thread_id) {
            return Ok(thread_id.to_string());
        }

        let mut params = self.resume_thread_params();
        params.insert("threadId".to_string(), Value::String(thread_id.to_string()));

        match self
            .rpc
            .request("thread/resume", Value::Object(params))
            .await
        {
            Ok(_) => {
                self.loaded_threads.insert(thread_id.to_string());
                Ok(thread_id.to_string())
            }
            Err(_) => self.new_thread().await,
        }
    }

    pub async fn run_turn(&mut self, thread_id: &str, user_text: &str) -> Result<TurnResult> {
        let response = self
            .rpc
            .request(
                "turn/start",
                json!({
                    "threadId": thread_id,
                    "input": [
                        {
                            "type": "text",
                            "text": user_text,
                        }
                    ]
                }),
            )
            .await?;

        let turn_id = response
            .get("turn")
            .and_then(Value::as_object)
            .and_then(|turn| turn.get("id"))
            .and_then(Value::as_str)
            .context("turn/start response missing turn.id")?
            .to_string();

        let mut deltas = String::new();
        let mut completed_messages: Vec<(Option<String>, String)> = Vec::new();
        let mut error_message = None;
        let mut completed = false;

        while let Some(notification) = self.rpc.next_notification().await {
            let method = notification
                .get("method")
                .and_then(Value::as_str)
                .unwrap_or_default();
            let params = match notification.get("params").and_then(Value::as_object) {
                Some(params) => params,
                None => continue,
            };

            let notification_thread_id = params
                .get("threadId")
                .and_then(Value::as_str)
                .unwrap_or_default();
            if notification_thread_id != thread_id {
                continue;
            }

            let notification_turn_id = params.get("turnId").and_then(Value::as_str);
            if let Some(notification_turn_id) = notification_turn_id {
                if notification_turn_id != turn_id {
                    continue;
                }
            }

            match method {
                "item/agentMessage/delta" => {
                    if let Some(delta) = params.get("delta").and_then(Value::as_str) {
                        deltas.push_str(delta);
                    }
                }
                "item/completed" => {
                    if let Some(item) = params.get("item").and_then(Value::as_object) {
                        if item.get("type").and_then(Value::as_str) == Some("agentMessage") {
                            let phase = item
                                .get("phase")
                                .and_then(Value::as_str)
                                .map(ToString::to_string);
                            let text = item
                                .get("text")
                                .and_then(Value::as_str)
                                .unwrap_or_default()
                                .to_string();
                            completed_messages.push((phase, text));
                        }
                    }
                }
                "error" => {
                    if let Some(error) = params.get("error").and_then(Value::as_object) {
                        error_message = Some(
                            error
                                .get("message")
                                .and_then(Value::as_str)
                                .unwrap_or("Codex turn failed")
                                .to_string(),
                        );
                    }
                }
                "turn/completed" => {
                    if params
                        .get("turn")
                        .and_then(Value::as_object)
                        .and_then(|turn| turn.get("status"))
                        .and_then(Value::as_str)
                        == Some("failed")
                        && error_message.is_none()
                    {
                        error_message = Some("Codex turn failed".to_string());
                    }
                    completed = true;
                    break;
                }
                _ => {}
            }
        }

        if !completed {
            return Err(anyhow::anyhow!(
                "Codex app server closed before the turn completed"
            ));
        }

        let final_messages: Vec<String> = completed_messages
            .iter()
            .filter_map(|(phase, text)| {
                if phase.as_deref() == Some("final_answer") && !text.trim().is_empty() {
                    Some(text.trim().to_string())
                } else {
                    None
                }
            })
            .collect();

        let text = if !final_messages.is_empty() {
            final_messages.join("\n\n")
        } else {
            let all_messages: Vec<String> = completed_messages
                .into_iter()
                .map(|(_, text)| text.trim().to_string())
                .filter(|text| !text.is_empty())
                .collect();
            if !all_messages.is_empty() {
                all_messages.join("\n\n")
            } else {
                deltas.trim().to_string()
            }
        };

        Ok(TurnResult {
            text,
            error_message,
        })
    }

    async fn initialize(&self) -> Result<()> {
        self.rpc
            .request(
                "initialize",
                json!({
                    "clientInfo": {
                        "name": "telegram-codex-relay",
                        "version": "0.1.0"
                    }
                }),
            )
            .await?;
        self.rpc.notify("initialized", None).await?;
        Ok(())
    }

    fn base_thread_params(&self) -> Map<String, Value> {
        let mut params = Map::new();
        params.insert(
            "cwd".to_string(),
            Value::String(self.config.codex_cwd.display().to_string()),
        );
        insert_optional_string(
            &mut params,
            "approvalPolicy",
            self.config.codex_approval_policy.clone(),
        );
        insert_optional_string(&mut params, "sandbox", self.config.codex_sandbox.clone());
        insert_optional_string(&mut params, "model", self.config.codex_model.clone());
        insert_optional_string(
            &mut params,
            "personality",
            self.config.codex_personality.clone(),
        );
        insert_optional_string(
            &mut params,
            "serviceTier",
            self.config.codex_service_tier.clone(),
        );
        insert_optional_string(
            &mut params,
            "baseInstructions",
            self.config.codex_base_instructions.clone(),
        );
        insert_optional_string(
            &mut params,
            "developerInstructions",
            self.config.codex_developer_instructions.clone(),
        );
        if let Some(config) = &self.config.codex_config {
            params.insert("config".to_string(), Value::Object(config.clone()));
        }
        params
    }

    fn new_thread_params(&self) -> Map<String, Value> {
        let mut params = self.base_thread_params();
        params.insert(
            "ephemeral".to_string(),
            Value::Bool(self.config.codex_ephemeral_threads),
        );
        params
    }

    fn resume_thread_params(&self) -> Map<String, Value> {
        self.base_thread_params()
    }
}

fn insert_optional_string(params: &mut Map<String, Value>, key: &str, value: Option<String>) {
    if let Some(value) = value {
        params.insert(key.to_string(), Value::String(value));
    }
}
