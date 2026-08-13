//! Windows Event Log source via `EvtSubscribe`.
//!
//! UNVERIFIED: this module was written against the documented
//! EvtSubscribe/EvtNext/EvtRender API shape (the same pull-model pattern
//! Microsoft's own C++ samples use for subscriptions), but has not been
//! compiled or run on a real Windows host — no Windows target toolchain
//! was available in the environment this was written in (confirmed: only
//! x86_64-unknown-linux-gnu std was installed, no rustup, no way to even
//! `cargo check --target x86_64-pc-windows-*`). Treat this as a first
//! draft to compile-check and test for real before trusting it. See
//! /docs/phase-1-runbook.md for what's actually been verified vs. not.

use super::{LineSender, RawLine};
use anyhow::{Context, Result};
use std::collections::HashMap;
use std::time::{SystemTime, UNIX_EPOCH};
use tokio::sync::mpsc as tokio_mpsc;

use windows::core::PCWSTR;
use windows::Win32::Foundation::{ERROR_NO_MORE_ITEMS, WAIT_OBJECT_0};
use windows::Win32::System::EventLog::{
    EvtClose, EvtNext, EvtRender, EvtRenderEventXml, EvtSubscribe, EVT_HANDLE,
    EVT_SUBSCRIBE_TO_FUTURE_EVENTS,
};
use windows::Win32::System::Threading::{CreateEventW, WaitForSingleObject};

/// Tails one or more Windows Event Log channels. Runs the blocking
/// EvtSubscribe/EvtNext calls on a dedicated OS thread per channel (via
/// `spawn_blocking`) and forwards parsed lines back over `tx`, same shape
/// as the journald source's subprocess-reading loop.
pub async fn run(channels: &[String], tx: LineSender) -> Result<()> {
    let channels = channels.to_vec();
    let (blocking_tx, mut blocking_rx) = tokio_mpsc::channel::<RawLine>(256);

    let handle = tokio::task::spawn_blocking(move || subscribe_all(&channels, blocking_tx));

    while let Some(line) = blocking_rx.recv().await {
        if tx.send(line).await.is_err() {
            break; // receiver dropped, agent is shutting down
        }
    }

    handle
        .await
        .context("windows event log subscription task panicked")??;
    Ok(())
}

/// One `std::thread` per channel, each blocked in its own
/// wait-then-drain loop. Simpler and still correct for the common case of
/// 1-3 channels; a single `WaitForMultipleObjects`-based dispatcher would
/// scale better to many channels but isn't needed for Phase 1's default
/// three (Application/System/Security).
fn subscribe_all(channels: &[String], tx: tokio_mpsc::Sender<RawLine>) -> Result<()> {
    let mut threads = Vec::with_capacity(channels.len());
    for channel in channels {
        let channel = channel.clone();
        let tx = tx.clone();
        threads.push(std::thread::spawn(move || subscribe_one(&channel, tx)));
    }
    for t in threads {
        t.join()
            .map_err(|_| anyhow::anyhow!("event log subscriber thread panicked"))??;
    }
    Ok(())
}

fn to_wide(s: &str) -> Vec<u16> {
    s.encode_utf16().chain(std::iter::once(0)).collect()
}

fn subscribe_one(channel: &str, tx: tokio_mpsc::Sender<RawLine>) -> Result<()> {
    unsafe {
        let signal_event = CreateEventW(None, true, false, None)
            .context("CreateEventW for subscription signal failed")?;

        let channel_wide = to_wide(channel);
        // NULL query (PCWSTR::null()) means "all events on this channel".
        // No callback (None) -- pull model via the signal event instead,
        // so this stays a plain loop rather than a Win32 callback that
        // would need to cross back into the tokio runtime.
        let subscription = EvtSubscribe(
            None,
            Some(signal_event),
            PCWSTR(channel_wide.as_ptr()),
            PCWSTR::null(),
            None,
            None,
            None,
            EVT_SUBSCRIBE_TO_FUTURE_EVENTS.0,
        )
        .context("EvtSubscribe failed")?;

        loop {
            let wait = WaitForSingleObject(signal_event, u32::MAX);
            if wait != WAIT_OBJECT_0 {
                anyhow::bail!("WaitForSingleObject on event log subscription failed");
            }

            loop {
                let mut events: [EVT_HANDLE; 16] = [EVT_HANDLE::default(); 16];
                let mut returned = 0u32;
                let next = EvtNext(subscription, &mut events, 0, 0, &mut returned);
                if let Err(err) = next {
                    if err.code() == ERROR_NO_MORE_ITEMS.into() {
                        break; // drained this batch; go back to waiting on the signal
                    }
                    return Err(err).context("EvtNext failed");
                }

                for &event in &events[..returned as usize] {
                    if let Some(raw) = render_event(event, channel) {
                        if tx.blocking_send(raw).is_err() {
                            let _ = EvtClose(event);
                            let _ = EvtClose(subscription);
                            return Ok(());
                        }
                    }
                    let _ = EvtClose(event);
                }
            }
        }
    }
}

fn render_event(event: EVT_HANDLE, channel: &str) -> Option<RawLine> {
    unsafe {
        let mut buffer_used = 0u32;
        let mut property_count = 0u32;
        // First call with a zero-length buffer to learn the required size.
        let _ = EvtRender(
            None,
            event,
            EvtRenderEventXml.0,
            0,
            None,
            &mut buffer_used,
            &mut property_count,
        );
        if buffer_used == 0 {
            return None;
        }

        let mut buffer = vec![0u16; (buffer_used as usize).div_ceil(2)];
        let rendered = EvtRender(
            None,
            event,
            EvtRenderEventXml.0,
            buffer_used,
            Some(buffer.as_mut_ptr() as *mut _),
            &mut buffer_used,
            &mut property_count,
        );
        if rendered.is_err() {
            return None;
        }

        let xml = String::from_utf16_lossy(&buffer);
        let xml = xml.trim_end_matches('\0');
        parse_event_xml(xml, channel)
    }
}

/// Minimal, deliberately non-validating extraction of the fields Phase 1
/// needs from the rendered event XML: EventID, Provider, Level, Computer,
/// Windows' own EventRecordID, and a best-effort message. Not a full XML
/// parser in the schema-aware sense -- uses `quick-xml`'s streaming
/// reader to pull specific elements/attributes rather than hand-rolled
/// string search, but doesn't attempt full EventData/UserData schema
/// awareness across every provider's custom shape. Worth revisiting once
/// this is running against real events from real providers.
fn parse_event_xml(xml: &str, channel: &str) -> Option<RawLine> {
    use quick_xml::events::Event as XmlEvent;
    use quick_xml::reader::Reader;

    let mut reader = Reader::from_str(xml);
    reader.config_mut().trim_text(true);

    let mut event_id = None;
    let mut provider = None;
    let mut level = None;
    let mut computer = None;
    let mut record_id = None;
    let mut event_data_values: Vec<String> = Vec::new();

    let mut current_tag: Option<String> = None;
    let mut buf = Vec::new();

    loop {
        match reader.read_event_into(&mut buf) {
            Ok(XmlEvent::Start(e)) | Ok(XmlEvent::Empty(e)) => {
                let name = local_name(&e);
                if name == "Provider" {
                    for attr in e.attributes().flatten() {
                        if attr.key.as_ref() == b"Name" {
                            provider = attr
                                .decode_and_unescape_value(reader.decoder())
                                .ok()
                                .map(|v| v.into_owned());
                        }
                    }
                }
                current_tag = Some(name);
            }
            Ok(XmlEvent::Text(t)) => {
                let text = t.unescape().unwrap_or_default().into_owned();
                match current_tag.as_deref() {
                    Some("EventID") => event_id = Some(text),
                    Some("Level") => level = text.parse::<u8>().ok(),
                    Some("Computer") => computer = Some(text),
                    Some("EventRecordID") => record_id = Some(text),
                    Some("Data") => event_data_values.push(text),
                    _ => {}
                }
            }
            Ok(XmlEvent::Eof) => break,
            Err(_) => return None,
            _ => {}
        }
        buf.clear();
    }

    // <EventData> commonly holds one or more <Data Name="...">value</Data>
    // elements rather than a single free-text message; joining them is a
    // reasonable Phase 1 default until per-provider message templates are
    // rendered properly. Real message-template rendering needs
    // EvtFormatMessage against the provider's message-table resource --
    // worth a follow-up, not required for a raw-passthrough-shaped record
    // (sentry_parser's raw fallback handles this fine either way).
    let message = if event_data_values.is_empty() {
        xml.to_string()
    } else {
        event_data_values.join(" | ")
    };

    let mut attributes = HashMap::new();
    if let Some(id) = &event_id {
        attributes.insert("winevt.event_id".to_string(), id.clone());
    }
    if let Some(p) = provider {
        attributes.insert("winevt.provider".to_string(), p);
    }
    attributes.insert("winevt.channel".to_string(), channel.to_string());
    if let Some(c) = computer {
        attributes.insert("winevt.computer".to_string(), c);
    }
    if let Some(r) = record_id {
        attributes.insert("winevt.record_number".to_string(), r);
    }

    let severity_hint = level.and_then(windows_level_to_syslog_severity);

    let timestamp_unix_nano = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos() as i64)
        .unwrap_or(0);

    Some(RawLine {
        line: message,
        timestamp_unix_nano,
        severity_hint,
        extra_attributes: attributes,
    })
}

fn local_name(e: &quick_xml::events::BytesStart) -> String {
    String::from_utf8_lossy(e.local_name().as_ref()).into_owned()
}

/// Maps Windows Event Log `Level` values (0=LogAlways, 1=Critical,
/// 2=Error, 3=Warning, 4=Informational, 5=Verbose) onto the same syslog
/// 0-7 severity scale `severity_hint` uses everywhere else in the agent,
/// so `main.rs`'s `to_pb_severity` needs no Windows-specific knowledge.
fn windows_level_to_syslog_severity(level: u8) -> Option<u8> {
    match level {
        1 => Some(2), // Critical -> crit
        2 => Some(3), // Error -> err
        3 => Some(4), // Warning -> warning
        4 => Some(6), // Informational -> info
        5 => Some(7), // Verbose -> debug
        _ => None,    // 0 (LogAlways) or unrecognized -- let the parser decide
    }
}
