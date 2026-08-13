use tokio::sync::mpsc;

/// A raw line read from a source, plus whatever metadata the source itself
/// already knows before the RFC 5424 parser ever sees it.
#[derive(Debug, Clone)]
pub struct RawLine {
    pub line: String,
    /// Unix epoch nanoseconds at time of read.
    pub timestamp_unix_nano: i64,
    /// Syslog severity (0-7) if the source already knows it independent of
    /// the line's own content — e.g. journald's PRIORITY field. When set,
    /// this takes precedence over whatever the RFC 5424 parser infers from
    /// the message text, since it comes from a more authoritative place.
    pub severity_hint: Option<u8>,
}

pub type LineSender = mpsc::Sender<RawLine>;

#[cfg(feature = "journald")]
pub mod journald;

#[cfg(feature = "file-tail")]
pub mod file_tail;
