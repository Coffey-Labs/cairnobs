use crate::pb::LogRecord;
use std::time::{Duration, Instant};

/// Buffers `LogRecord`s and signals when to flush, either because the
/// buffer hit `max_size` (checked on every push) or because
/// `flush_interval` elapsed since the last flush (checked by the caller via
/// `poll_timeout` on a timer tick). Not thread-safe by design — one
/// batcher per agent, driven from a single async task's select loop.
pub struct Batcher {
    max_size: usize,
    flush_interval: Duration,
    buf: Vec<LogRecord>,
    last_flush: Instant,
}

impl Batcher {
    pub fn new(max_size: usize, flush_interval: Duration) -> Self {
        Self {
            max_size,
            flush_interval,
            buf: Vec::with_capacity(max_size),
            last_flush: Instant::now(),
        }
    }

    /// Push a record. Returns the drained batch if this push filled the
    /// buffer to `max_size`.
    pub fn push(&mut self, record: LogRecord) -> Option<Vec<LogRecord>> {
        self.buf.push(record);
        if self.buf.len() >= self.max_size {
            Some(self.drain())
        } else {
            None
        }
    }

    /// Call periodically (e.g. from a timer tick). Returns the drained
    /// batch if the flush interval has elapsed and there's anything
    /// buffered.
    pub fn poll_timeout(&mut self) -> Option<Vec<LogRecord>> {
        if !self.buf.is_empty() && self.last_flush.elapsed() >= self.flush_interval {
            Some(self.drain())
        } else {
            None
        }
    }

    fn drain(&mut self) -> Vec<LogRecord> {
        self.last_flush = Instant::now();
        std::mem::replace(&mut self.buf, Vec::with_capacity(self.max_size))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn rec(msg: &str) -> LogRecord {
        LogRecord {
            timestamp_unix_nano: 0,
            host: "h".into(),
            service: "s".into(),
            severity: 0,
            message: msg.into(),
            attributes: Default::default(),
            record_id: String::new(),
        }
    }

    #[test]
    fn flushes_on_size() {
        let mut b = Batcher::new(2, Duration::from_secs(999));
        assert!(b.push(rec("a")).is_none());
        let batch = b.push(rec("b")).expect("should flush at max_size");
        assert_eq!(batch.len(), 2);
        assert_eq!(batch[0].message, "a");
        assert_eq!(batch[1].message, "b");
    }

    #[test]
    fn buffer_empty_after_size_flush() {
        let mut b = Batcher::new(1, Duration::from_secs(999));
        b.push(rec("a")).expect("flush at max_size 1");
        assert!(b.poll_timeout().is_none(), "buffer should be empty post-flush");
    }

    #[test]
    fn flushes_on_timeout() {
        let mut b = Batcher::new(100, Duration::from_millis(10));
        assert!(b.push(rec("a")).is_none());
        std::thread::sleep(Duration::from_millis(30));
        let batch = b.poll_timeout().expect("should flush after timeout");
        assert_eq!(batch.len(), 1);
    }

    #[test]
    fn no_flush_when_buffer_empty() {
        let mut b = Batcher::new(10, Duration::from_millis(1));
        std::thread::sleep(Duration::from_millis(5));
        assert!(b.poll_timeout().is_none());
    }

    #[test]
    fn no_flush_before_timeout_elapsed() {
        let mut b = Batcher::new(10, Duration::from_secs(999));
        b.push(rec("a"));
        assert!(b.poll_timeout().is_none());
    }
}
