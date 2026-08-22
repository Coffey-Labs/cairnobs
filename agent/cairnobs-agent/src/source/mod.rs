use std::collections::HashMap;
use tokio::sync::mpsc;

/// A raw line read from a source, plus whatever metadata the source itself
/// already knows before the RFC 5424 parser ever sees it.
#[derive(Debug, Clone)]
pub struct RawLine {
    pub line: String,
    /// Unix epoch nanoseconds at time of read.
    pub timestamp_unix_nano: i64,
    /// Syslog severity (0-7) if the source already knows it independent of
    /// the line's own content — e.g. journald's PRIORITY field, or a
    /// Windows Event Log Level mapped onto the same scale. When set, this
    /// takes precedence over whatever the RFC 5424 parser infers from the
    /// message text, since it comes from a more authoritative place.
    pub severity_hint: Option<u8>,
    /// Structured fields the source already knows, independent of the raw
    /// message text — e.g. Windows Event Log's EventID/Provider/Channel.
    /// Merged into the record's attributes alongside whatever the RFC 5424
    /// parser extracts from `line`; on key collision, these win, since
    /// they also come from a more authoritative place than text parsing.
    /// Sources that have nothing to add (journald, file-tail) just leave
    /// this empty.
    pub extra_attributes: HashMap<String, String>,
}

pub type LineSender = mpsc::Sender<RawLine>;

#[cfg(all(feature = "journald", target_os = "linux"))]
pub mod journald;

#[cfg(feature = "file-tail")]
pub mod file_tail;

#[cfg(all(feature = "windows-eventlog", target_os = "windows"))]
pub mod windows_eventlog;

#[cfg(all(feature = "etw", target_os = "windows"))]
pub mod etw;
