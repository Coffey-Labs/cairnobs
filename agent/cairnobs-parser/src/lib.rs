//! Minimal RFC 5424 syslog parser with raw-passthrough fallback.
//!
//! This is intentionally not a complete RFC 5424 implementation (no BOM
//! handling on MSG, "-" nil markers are kept as literal strings rather than
//! mapped to `None`). It's the Phase 0 minimum: parse what's clearly
//! structured syslog, and never fail a log line outright — anything that
//! doesn't match the grammar becomes a raw passthrough record instead of
//! being dropped.

use std::collections::BTreeMap;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParsedLine {
    /// Syslog facility (0-23), present only when RFC 5424 framing parsed.
    pub facility: Option<u8>,
    /// Syslog severity (0=emergency .. 7=debug), present only when RFC 5424
    /// framing parsed.
    pub severity: Option<u8>,
    /// Structured fields extracted from the PRI/HEADER/STRUCTURED-DATA
    /// portions. Empty when the raw-passthrough fallback fires.
    pub attributes: BTreeMap<String, String>,
    /// The MSG portion when RFC 5424 parsing succeeded, otherwise the
    /// original, unmodified line.
    pub message: String,
}

/// Parse a single log line. Never fails: falls back to a raw passthrough
/// `ParsedLine` (no facility/severity, empty attributes, message = input)
/// when the line doesn't match RFC 5424 framing.
pub fn parse(line: &str) -> ParsedLine {
    parse_rfc5424(line).unwrap_or_else(|| ParsedLine {
        facility: None,
        severity: None,
        attributes: BTreeMap::new(),
        message: line.to_string(),
    })
}

struct Scanner<'a> {
    chars: std::iter::Peekable<std::str::Chars<'a>>,
}

impl<'a> Scanner<'a> {
    fn new(s: &'a str) -> Self {
        Scanner {
            chars: s.chars().peekable(),
        }
    }

    fn peek(&mut self) -> Option<char> {
        self.chars.peek().copied()
    }

    fn next(&mut self) -> Option<char> {
        self.chars.next()
    }

    fn expect(&mut self, c: char) -> Option<()> {
        if self.next()? == c {
            Some(())
        } else {
            None
        }
    }

    fn take_while<F: Fn(char) -> bool>(&mut self, f: F) -> String {
        let mut out = String::new();
        while let Some(c) = self.peek() {
            if f(c) {
                out.push(c);
                self.next();
            } else {
                break;
            }
        }
        out
    }

    fn skip_one_space(&mut self) -> Option<()> {
        self.expect(' ')
    }
}

fn parse_rfc5424(line: &str) -> Option<ParsedLine> {
    let mut sc = Scanner::new(line);

    sc.expect('<')?;
    let pri_str = sc.take_while(|c| c.is_ascii_digit());
    if pri_str.is_empty() || pri_str.len() > 3 {
        return None;
    }
    sc.expect('>')?;
    let pri: u16 = pri_str.parse().ok()?;
    if pri > 191 {
        return None;
    }
    let facility = (pri / 8) as u8;
    let severity = (pri % 8) as u8;

    let version = sc.take_while(|c| c.is_ascii_digit());
    if version.is_empty() {
        return None;
    }
    sc.skip_one_space()?;

    let timestamp = sc.take_while(|c| c != ' ');
    if timestamp.is_empty() {
        return None;
    }
    sc.skip_one_space()?;

    let hostname = sc.take_while(|c| c != ' ');
    if hostname.is_empty() {
        return None;
    }
    sc.skip_one_space()?;

    let app_name = sc.take_while(|c| c != ' ');
    if app_name.is_empty() {
        return None;
    }
    sc.skip_one_space()?;

    let procid = sc.take_while(|c| c != ' ');
    if procid.is_empty() {
        return None;
    }
    sc.skip_one_space()?;

    let msgid = sc.take_while(|c| c != ' ');
    if msgid.is_empty() {
        return None;
    }
    sc.skip_one_space()?;

    let mut sd_pairs: Vec<(String, String, String)> = Vec::new();
    match sc.peek() {
        Some('-') => {
            sc.next();
        }
        Some('[') => loop {
            if sc.peek() != Some('[') {
                break;
            }
            sc.next();
            let sd_id = sc.take_while(|c| c != ' ' && c != ']');
            if sd_id.is_empty() {
                return None;
            }
            loop {
                match sc.peek() {
                    Some(' ') => {
                        sc.next();
                        let name = sc.take_while(|c| c != '=');
                        sc.expect('=')?;
                        sc.expect('"')?;
                        let mut val = String::new();
                        loop {
                            match sc.next() {
                                Some('\\') => val.push(sc.next()?),
                                Some('"') => break,
                                Some(c) => val.push(c),
                                None => return None,
                            }
                        }
                        sd_pairs.push((sd_id.clone(), name, val));
                    }
                    Some(']') => {
                        sc.next();
                        break;
                    }
                    _ => return None,
                }
            }
        },
        _ => return None,
    }

    let message = if sc.peek() == Some(' ') {
        sc.next();
        sc.take_while(|_| true)
    } else {
        String::new()
    };

    let mut attributes = BTreeMap::new();
    attributes.insert("syslog.version".to_string(), version);
    attributes.insert("syslog.timestamp".to_string(), timestamp);
    attributes.insert("syslog.hostname".to_string(), hostname);
    attributes.insert("syslog.app_name".to_string(), app_name);
    attributes.insert("syslog.procid".to_string(), procid);
    attributes.insert("syslog.msgid".to_string(), msgid);
    for (sd_id, name, val) in sd_pairs {
        attributes.insert(format!("{sd_id}.{name}"), val);
    }

    Some(ParsedLine {
        facility: Some(facility),
        severity: Some(severity),
        attributes,
        message,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_full_rfc5424_with_structured_data() {
        let line = r#"<165>1 2003-10-11T22:14:15.003Z mymachine.example.com evntslp - ID47 [exampleSDID@32473 iut="3" eventSource="Application" eventID="1011"] An application event log entry"#;
        let p = parse(line);
        assert_eq!(p.facility, Some(20));
        assert_eq!(p.severity, Some(5));
        assert_eq!(p.message, "An application event log entry");
        assert_eq!(
            p.attributes.get("exampleSDID@32473.iut"),
            Some(&"3".to_string())
        );
        assert_eq!(
            p.attributes.get("exampleSDID@32473.eventSource"),
            Some(&"Application".to_string())
        );
        assert_eq!(
            p.attributes.get("syslog.hostname"),
            Some(&"mymachine.example.com".to_string())
        );
    }

    #[test]
    fn parses_nil_structured_data_and_fields() {
        let line = "<34>1 2003-10-11T22:14:15.003Z mymachine su - ID47 - 'su root' failed";
        let p = parse(line);
        assert_eq!(p.facility, Some(4));
        assert_eq!(p.severity, Some(2));
        assert_eq!(p.attributes.get("syslog.procid"), Some(&"-".to_string()));
        assert_eq!(p.message, "'su root' failed");
    }

    #[test]
    fn parses_multiple_structured_data_elements() {
        let line = r#"<165>1 2003-10-11T22:14:15.003Z host app - ID47 [a@1 k="v"][b@1 k2="v2"] msg"#;
        let p = parse(line);
        assert_eq!(p.attributes.get("a@1.k"), Some(&"v".to_string()));
        assert_eq!(p.attributes.get("b@1.k2"), Some(&"v2".to_string()));
        assert_eq!(p.message, "msg");
    }

    #[test]
    fn handles_escaped_quote_in_param_value() {
        let line = r#"<165>1 2003-10-11T22:14:15.003Z host app - ID47 [a@1 k="has \"quote\" inside"] msg"#;
        let p = parse(line);
        assert_eq!(
            p.attributes.get("a@1.k"),
            Some(&"has \"quote\" inside".to_string())
        );
    }

    #[test]
    fn falls_back_to_raw_passthrough_for_non_syslog_line() {
        let line = "this is just a plain log line, not syslog at all";
        let p = parse(line);
        assert_eq!(p.facility, None);
        assert_eq!(p.severity, None);
        assert!(p.attributes.is_empty());
        assert_eq!(p.message, line);
    }

    #[test]
    fn falls_back_to_raw_passthrough_for_malformed_pri() {
        let line = "<abc>1 2003-10-11T22:14:15.003Z host app - ID47 - msg";
        let p = parse(line);
        assert_eq!(p.facility, None);
        assert_eq!(p.message, line);
    }

    #[test]
    fn falls_back_when_structured_data_missing() {
        // Missing the required "-" or "[...]" for STRUCTURED-DATA.
        let line = "<34>1 2003-10-11T22:14:15.003Z host app 123 ID47";
        let p = parse(line);
        assert_eq!(p.facility, None);
        assert_eq!(p.message, line);
    }
}
