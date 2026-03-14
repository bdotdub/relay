use anyhow::{anyhow, Context, Result};
use reqwest::Client;
use serde::{de::DeserializeOwned, Deserialize, Serialize};

#[derive(Debug, Clone)]
pub struct TelegramClient {
    http: Client,
    base_url: String,
}

#[derive(Debug, Deserialize)]
pub struct Update {
    pub update_id: i64,
    pub message: Option<Message>,
}

#[derive(Debug, Deserialize)]
pub struct Message {
    pub message_id: i64,
    pub chat: Chat,
    pub text: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct Chat {
    pub id: i64,
}

#[derive(Debug, Deserialize)]
struct ApiResponse<T> {
    ok: bool,
    result: Option<T>,
    description: Option<String>,
}

#[derive(Debug, Serialize)]
struct DeleteWebhookRequest {
    drop_pending_updates: bool,
}

#[derive(Debug, Serialize)]
struct GetUpdatesRequest<'a> {
    #[serde(skip_serializing_if = "Option::is_none")]
    offset: Option<i64>,
    timeout: u64,
    allowed_updates: &'a [&'a str],
}

#[derive(Debug, Serialize)]
struct SendMessageRequest<'a> {
    chat_id: i64,
    text: &'a str,
    disable_web_page_preview: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    reply_to_message_id: Option<i64>,
}

impl TelegramClient {
    pub fn new(token: &str) -> Result<Self> {
        Ok(Self {
            http: Client::builder()
                .timeout(std::time::Duration::from_secs(60))
                .build()
                .context("failed to build reqwest client")?,
            base_url: format!("https://api.telegram.org/bot{token}"),
        })
    }

    pub async fn delete_webhook(&self, drop_pending_updates: bool) -> Result<()> {
        let payload = DeleteWebhookRequest {
            drop_pending_updates,
        };
        let _: serde_json::Value = self.call("deleteWebhook", &payload).await?;
        Ok(())
    }

    pub async fn get_updates(&self, offset: Option<i64>, timeout: u64) -> Result<Vec<Update>> {
        let payload = GetUpdatesRequest {
            offset,
            timeout,
            allowed_updates: &["message"],
        };
        self.call("getUpdates", &payload).await
    }

    pub async fn send_message(
        &self,
        chat_id: i64,
        text: &str,
        reply_to_message_id: Option<i64>,
    ) -> Result<()> {
        let payload = SendMessageRequest {
            chat_id,
            text,
            disable_web_page_preview: true,
            reply_to_message_id,
        };
        let _: serde_json::Value = self.call("sendMessage", &payload).await?;
        Ok(())
    }

    async fn call<T, B>(&self, method: &str, body: &B) -> Result<T>
    where
        T: DeserializeOwned,
        B: Serialize + ?Sized,
    {
        let response = self
            .http
            .post(format!("{}/{}", self.base_url, method))
            .json(body)
            .send()
            .await
            .with_context(|| format!("Telegram API request '{method}' failed"))?;

        let response = response
            .error_for_status()
            .with_context(|| format!("Telegram API returned HTTP error for '{method}'"))?;

        let body: ApiResponse<T> = response
            .json()
            .await
            .with_context(|| format!("failed to decode Telegram response for '{method}'"))?;

        if body.ok {
            body.result.ok_or_else(|| anyhow!("Telegram response missing result"))
        } else {
            Err(anyhow!(
                body.description
                    .unwrap_or_else(|| "Telegram API returned an error".to_string())
            ))
        }
    }
}

pub fn chunk_message(text: &str, limit: usize) -> Vec<String> {
    if limit == 0 {
        return vec!["(empty response)".to_string()];
    }

    let stripped = text.trim();
    if stripped.is_empty() {
        return vec!["(empty response)".to_string()];
    }
    if stripped.len() <= limit {
        return vec![stripped.to_string()];
    }

    let mut chunks = Vec::new();
    let mut current = String::new();

    for paragraph in stripped.split("\n\n") {
        let candidate = paragraph.trim();
        if candidate.is_empty() {
            continue;
        }

        let joined = if current.is_empty() {
            candidate.to_string()
        } else {
            format!("{current}\n\n{candidate}")
        };

        if joined.len() <= limit {
            current = joined;
            continue;
        }

        if !current.is_empty() {
            chunks.push(std::mem::take(&mut current));
        }

        if candidate.len() <= limit {
            current = candidate.to_string();
            continue;
        }

        for line in candidate.lines() {
            let line = line.trim();
            if line.is_empty() {
                continue;
            }

            let joined_line = if current.is_empty() {
                line.to_string()
            } else {
                format!("{current}\n{line}")
            };

            if joined_line.len() <= limit {
                current = joined_line;
                continue;
            }

            if !current.is_empty() {
                chunks.push(std::mem::take(&mut current));
            }

            let mut remainder = line;
            while remainder.len() > limit {
                let split_at = previous_char_boundary(remainder, limit);
                chunks.push(remainder[..split_at].to_string());
                remainder = &remainder[split_at..];
            }
            current = remainder.to_string();
        }
    }

    if !current.is_empty() {
        chunks.push(current);
    }

    chunks
}

fn previous_char_boundary(text: &str, max_bytes: usize) -> usize {
    let mut index = max_bytes.min(text.len());
    while !text.is_char_boundary(index) {
        index -= 1;
    }
    index
}

#[cfg(test)]
mod tests {
    use super::chunk_message;

    #[test]
    fn chunked_messages_stay_within_limit() {
        let text = format!("Paragraph one.\n\n{}\n{}", "x".repeat(50), "y".repeat(50));
        let chunks = chunk_message(&text, 40);
        assert!(chunks.len() > 1);
        assert!(chunks.iter().all(|chunk| chunk.len() <= 40));
    }

    #[test]
    fn empty_messages_receive_placeholder() {
        assert_eq!(chunk_message("   ", 20), vec!["(empty response)"]);
    }
}
