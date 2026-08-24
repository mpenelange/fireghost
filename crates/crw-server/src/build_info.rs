//! Build identity embedded in `crw-server` and its health responses.
//!
//! Release/container builds set these values through Docker build arguments.
//! Local Cargo builds intentionally fall back to explicit `unknown` values so
//! an unlabelled artifact can never be mistaken for a traceable release.

/// Cargo package version of the server source.
pub const PACKAGE_VERSION: &str = env!("CARGO_PKG_VERSION");

/// Operator-facing artifact version (for example `1.2.0-fw.3`).
pub const VERSION: &str = match option_env!("CRW_BUILD_VERSION") {
    Some(value) => value,
    None => PACKAGE_VERSION,
};

/// Full source revision used for the build.
pub const REVISION: &str = match option_env!("CRW_BUILD_REVISION") {
    Some(value) => value,
    None => "unknown",
};

/// UTC build timestamp supplied by the release process.
pub const BUILD_DATE: &str = match option_env!("CRW_BUILD_DATE") {
    Some(value) => value,
    None => "unknown",
};

/// Human-readable, stable version output for diagnostics.
pub fn display() -> String {
    format!(
        "crw-server {VERSION} (package {PACKAGE_VERSION}, revision {REVISION}, built {BUILD_DATE})"
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn display_contains_every_identity_surface() {
        let rendered = display();
        assert!(rendered.contains(VERSION));
        assert!(rendered.contains(PACKAGE_VERSION));
        assert!(rendered.contains(REVISION));
        assert!(rendered.contains(BUILD_DATE));
    }
}
