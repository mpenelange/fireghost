use thiserror::Error;

#[derive(Debug, Error)]
pub enum CrwError {
    #[error("HTTP request failed: {0}")]
    HttpError(String),

    #[error("Target unreachable: {0}")]
    TargetUnreachable(String),

    #[error("URL parse error: {0}")]
    UrlParseError(#[from] url::ParseError),

    #[error("Invalid request: {0}")]
    InvalidRequest(String),

    #[error("Renderer error: {0}")]
    RendererError(String),

    #[error("Extraction error: {0}")]
    ExtractionError(String),

    /// The target served a sign-in page in place of the requested content.
    #[error("Login required: {0}")]
    LoginRequired(String),

    #[error("Crawl error: {0}")]
    CrawlError(String),

    #[error("Timeout after {0}ms")]
    Timeout(u64),

    #[error("Config error: {0}")]
    ConfigError(String),

    #[error("Not found: {0}")]
    NotFound(String),

    #[error("Rate limited")]
    RateLimited,

    #[error("{0}")]
    SearchDisabled(String),

    #[error("{0}")]
    Internal(String),

    #[error("Renderer pool shutting down")]
    Shutdown,
}

impl CrwError {
    /// Machine-readable error code for API consumers.
    pub fn error_code(&self) -> &'static str {
        match self {
            CrwError::HttpError(_) => "http_error",
            CrwError::TargetUnreachable(_) => "target_unreachable",
            CrwError::UrlParseError(_) => "invalid_url",
            CrwError::InvalidRequest(_) => "invalid_request",
            CrwError::RendererError(_) => "renderer_error",
            CrwError::ExtractionError(_) => "extraction_error",
            CrwError::LoginRequired(_) => "login_required",
            CrwError::CrawlError(_) => "crawl_error",
            CrwError::Timeout(_) => "timeout",
            CrwError::ConfigError(_) => "config_error",
            CrwError::NotFound(_) => "not_found",
            CrwError::RateLimited => "rate_limited",
            CrwError::SearchDisabled(_) => "search_disabled",
            CrwError::Internal(_) => "internal_error",
            CrwError::Shutdown => "shutdown",
        }
    }
}

pub type CrwResult<T> = Result<T, CrwError>;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn login_required_has_stable_code_and_neutral_message() {
        let err = CrwError::LoginRequired("reddit.com served a sign-in page".into());
        assert_eq!(err.error_code(), "login_required");
        // Callers such as the Fireghost router retry anti-bot, timeout and
        // rate-limit wording against a cloud fallback; a sign-in page is final.
        let message = err.to_string().to_lowercase();
        for retryable in [
            "blocked",
            "anti-bot",
            "challenge",
            "captcha",
            "403",
            "forbidden",
            "timeout",
            "rate limit",
        ] {
            assert!(
                !message.contains(retryable),
                "{message:?} contains {retryable:?}"
            );
        }
    }
}
