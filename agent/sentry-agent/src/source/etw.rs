//! ETW (Event Tracing for Windows) source: a real-time trace session
//! subscribed to specific provider GUIDs.
//!
//! UNVERIFIED, and the highest-risk file in this whole Windows integration
//! -- more so than windows_eventlog.rs. `EVENT_TRACE_PROPERTIES` requires
//! a variable-length buffer appended after the fixed struct (a classic C
//! "flexible array member" pattern for LoggerName), which is exactly the
//! kind of FFI layout detail most likely to be subtly wrong without a
//! Windows toolchain to actually compile and run this against. No Windows
//! target was available in the environment this was written in -- see the
//! module-level note in windows_eventlog.rs for what that means. Compile-
//! check and test this file specifically, first, before trusting any of
//! it.
//!
//! Providers are configured by **GUID**, not friendly name (e.g.
//! `"{22FB2CD6-0E7B-422B-A0C7-2FAD1FD0E716}"`) -- ETW's own
//! `EnableTraceEx2` API takes a GUID, not a name, and there's no simple
//! name-to-GUID resolution in the raw ETW API (that needs the separate TDH
//! provider-enumeration API, not implemented here). Look up a provider's
//! GUID with `logman query providers "<Friendly Name>"`.
//!
//! Message extraction here is deliberately limited to what's available
//! directly on `EVENT_RECORD`'s header (ProviderId, EventID, Level,
//! Keywords, timestamp, process/thread ID) -- no TDH-based property
//! decoding or message-template rendering (`TdhGetEventInformation`),
//! which is a meaningfully larger undertaking left for a follow-up. This
//! gives real session/provider/callback plumbing with a coarse message,
//! not full structured event decoding.

use super::{LineSender, RawLine};
use anyhow::{Context, Result};
use std::collections::HashMap;
use std::ffi::c_void;
use std::sync::mpsc as std_mpsc;
use std::time::{SystemTime, UNIX_EPOCH};
use tokio::sync::mpsc as tokio_mpsc;

use windows::core::{GUID, PCWSTR};
use windows::Win32::System::Diagnostics::Etw::{
    CloseTrace, ControlTraceW, EnableTraceEx2, OpenTraceW, ProcessTrace, StartTraceW,
    EVENT_CONTROL_CODE_ENABLE_PROVIDER, EVENT_RECORD, EVENT_TRACE_CONTROL_STOP,
    EVENT_TRACE_LOGFILEW, EVENT_TRACE_LOGFILEW_0, EVENT_TRACE_LOGFILEW_1,
    EVENT_TRACE_PROPERTIES, EVENT_TRACE_REAL_TIME_MODE, PROCESS_TRACE_MODE_EVENT_RECORD,
    PROCESS_TRACE_MODE_REAL_TIME, TRACE_LEVEL_VERBOSE,
};

const SESSION_NAME: &str = "SentryAgentEtw";

pub async fn run(providers: &[String], tx: LineSender) -> Result<()> {
    let providers = providers.to_vec();
    let (blocking_tx, mut blocking_rx) = tokio_mpsc::channel::<RawLine>(256);

    let handle = tokio::task::spawn_blocking(move || run_session(&providers, blocking_tx));

    while let Some(line) = blocking_rx.recv().await {
        if tx.send(line).await.is_err() {
            break;
        }
    }

    handle.await.context("ETW session task panicked")??;
    Ok(())
}

fn to_wide(s: &str) -> Vec<u16> {
    s.encode_utf16().chain(std::iter::once(0)).collect()
}

/// Thread-local-ish channel used to get the async sender into the
/// C-callable `event_record_callback`, which has a fixed extern "system"
/// signature and can't capture a closure. Set once per `run_session` call
/// before `ProcessTrace` starts invoking the callback.
thread_local! {
    static CALLBACK_TX: std::cell::RefCell<Option<tokio_mpsc::Sender<RawLine>>> =
        const { std::cell::RefCell::new(None) };
}

fn run_session(providers: &[String], tx: tokio_mpsc::Sender<RawLine>) -> Result<()> {
    let guids: Vec<GUID> = providers
        .iter()
        .map(|p| GUID::try_from(p.as_str()).with_context(|| format!("invalid provider GUID: {p}")))
        .collect::<Result<_>>()?;

    unsafe {
        let session_handle = start_session()?;
        for guid in &guids {
            enable_provider(session_handle, guid)?;
        }

        // ProcessTrace runs the consumer loop on *this* thread until
        // CloseTrace is called (from the callback, or from another
        // thread against the same handle) -- there's no separate
        // "shutdown channel" here because the agent's top-level shutdown
        // path currently aborts the whole spawn_blocking task rather
        // than signaling sources to stop gracefully (same as the other
        // sources today).
        CALLBACK_TX.with(|cell| *cell.borrow_mut() = Some(tx));

        let mut logfile = EVENT_TRACE_LOGFILEW::default();
        let mut session_name_wide = to_wide(SESSION_NAME);
        logfile.LoggerName = PCWSTR(session_name_wide.as_mut_ptr());
        logfile.Anonymous1 = EVENT_TRACE_LOGFILEW_0 {
            ProcessTraceMode: PROCESS_TRACE_MODE_REAL_TIME.0 | PROCESS_TRACE_MODE_EVENT_RECORD.0,
        };
        logfile.Anonymous2 = EVENT_TRACE_LOGFILEW_1 {
            EventRecordCallback: Some(event_record_callback),
        };

        let trace_handle = OpenTraceW(&mut logfile);
        if trace_handle.0 == u64::MAX as usize {
            anyhow::bail!("OpenTraceW failed");
        }

        let result = ProcessTrace(&[trace_handle], None, None);

        let _ = CloseTrace(trace_handle);
        stop_session(session_handle);

        result.ok().context("ProcessTrace failed")?;
    }

    Ok(())
}

unsafe fn start_session() -> Result<windows::Win32::System::Diagnostics::Etw::CONTROLTRACE_HANDLE> {
    // EVENT_TRACE_PROPERTIES needs a trailing buffer (appended after the
    // fixed struct) for the session's LoggerName -- this is the flexible-
    // array-member pattern flagged in the module doc comment as the
    // highest-risk detail in this file. LogFileNameOffset is left 0 (no
    // log file; real-time only).
    const LOGGER_NAME_CAPACITY: usize = 256;
    let total_size = std::mem::size_of::<EVENT_TRACE_PROPERTIES>() + LOGGER_NAME_CAPACITY * 2;
    let mut buffer = vec![0u8; total_size];

    let props = buffer.as_mut_ptr() as *mut EVENT_TRACE_PROPERTIES;
    (*props).Wnode.BufferSize = total_size as u32;
    (*props).Wnode.Flags = windows::Win32::System::Diagnostics::Etw::WNODE_FLAG_TRACED_GUID;
    (*props).LogFileMode = EVENT_TRACE_REAL_TIME_MODE;
    (*props).LoggerNameOffset = std::mem::size_of::<EVENT_TRACE_PROPERTIES>() as u32;

    let session_name_wide = to_wide(SESSION_NAME);
    let mut session_handle = Default::default();
    StartTraceW(
        &mut session_handle,
        PCWSTR(session_name_wide.as_ptr()),
        props,
    )
    .ok()
    .context("StartTraceW failed")?;

    Ok(session_handle)
}

unsafe fn enable_provider(
    session_handle: windows::Win32::System::Diagnostics::Etw::CONTROLTRACE_HANDLE,
    guid: &GUID,
) -> Result<()> {
    EnableTraceEx2(
        session_handle,
        guid,
        EVENT_CONTROL_CODE_ENABLE_PROVIDER.0,
        TRACE_LEVEL_VERBOSE as u8,
        0,
        0,
        0,
        None,
    )
    .ok()
    .with_context(|| format!("EnableTraceEx2 failed for provider {guid:?}"))
}

unsafe fn stop_session(session_handle: windows::Win32::System::Diagnostics::Etw::CONTROLTRACE_HANDLE) {
    let mut buffer = vec![0u8; std::mem::size_of::<EVENT_TRACE_PROPERTIES>() + 512];
    let props = buffer.as_mut_ptr() as *mut EVENT_TRACE_PROPERTIES;
    (*props).Wnode.BufferSize = buffer.len() as u32;
    let _ = ControlTraceW(session_handle, PCWSTR::null(), props, EVENT_TRACE_CONTROL_STOP);
}

/// `extern "system"` callback ETW invokes per event during `ProcessTrace`.
/// Deliberately minimal: header fields only, no TDH property decoding
/// (see module doc comment).
unsafe extern "system" fn event_record_callback(record: *mut EVENT_RECORD) {
    if record.is_null() {
        return;
    }
    let record = &*record;
    let header = &record.EventHeader;

    let mut attributes = HashMap::new();
    attributes.insert(
        "etw.provider_guid".to_string(),
        format!("{:?}", header.ProviderId),
    );
    attributes.insert("etw.event_id".to_string(), header.EventDescriptor.Id.to_string());
    attributes.insert(
        "etw.opcode".to_string(),
        header.EventDescriptor.Opcode.to_string(),
    );
    attributes.insert(
        "etw.task".to_string(),
        header.EventDescriptor.Task.to_string(),
    );
    attributes.insert("etw.process_id".to_string(), header.ProcessId.to_string());
    attributes.insert("etw.thread_id".to_string(), header.ThreadId.to_string());

    let severity_hint = etw_level_to_syslog_severity(header.EventDescriptor.Level);

    let timestamp_unix_nano = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos() as i64)
        .unwrap_or(0);

    // No TDH-based message rendering (see module doc comment) -- this is
    // a coarse, structured summary rather than a human-authored message.
    // Downstream (sentry_parser's raw-passthrough fallback) handles a
    // non-RFC5424 line like this the same as any other raw line.
    let message = format!(
        "ETW event: provider={:?} id={} level={}",
        header.ProviderId, header.EventDescriptor.Id, header.EventDescriptor.Level
    );

    let raw = RawLine {
        line: message,
        timestamp_unix_nano,
        severity_hint,
        extra_attributes: attributes,
    };

    CALLBACK_TX.with(|cell| {
        if let Some(tx) = cell.borrow().as_ref() {
            let _ = tx.blocking_send(raw);
        }
    });
}

/// Maps ETW `Level` (0=LogAlways/Verbose-ish through 5=Verbose, following
/// the same TRACE_LEVEL_* scale as windows_eventlog's Level values) onto
/// the syslog 0-7 scale, same reasoning as windows_eventlog.rs.
fn etw_level_to_syslog_severity(level: u8) -> Option<u8> {
    match level {
        1 => Some(2), // Critical -> crit
        2 => Some(3), // Error -> err
        3 => Some(4), // Warning -> warning
        4 => Some(6), // Informational -> info
        5 => Some(7), // Verbose -> debug
        _ => None,
    }
}
