use anyhow::{Context, Result};
use std::fs;
use std::process::Command;
use std::time::Duration;

pub struct Metrics {
    pub cpu_percent: f64,
    pub mem_used_bytes: u64,
    pub mem_total_bytes: u64,
    pub disk_used_bytes: u64,
    pub disk_total_bytes: u64,
    /// The rest of these are static-or-slow-changing context, not
    /// utilization numbers -- sent alongside the utilization fields on
    /// the same record (rather than as a separate one-off record) so a
    /// viewer never has to correlate two different samples to answer
    /// "is 21% CPU busy or idle for this box" (needs core count) or
    /// "is this usage normal" (needs how long it's been running).
    pub cpu_cores: u32,
    pub os_name: String,
    pub kernel_version: String,
    pub arch: &'static str,
    pub uptime_seconds: u64,
    /// Non-loopback, non-link-local addresses only -- a host's `fe80::/10`
    /// and `127.0.0.1`/`::1` are never what a viewer means by "this
    /// host's IP", and would just add noise. Sorted and deduplicated,
    /// but otherwise unfiltered: a multi-NIC host reports every address
    /// it has, not just one "primary" guess (there's no reliable way to
    /// pick a single "the" address from userspace without also knowing
    /// which interface actually carries this host's traffic).
    pub ipv4_addresses: Vec<String>,
    pub ipv6_addresses: Vec<String>,
}

/// Collects a point-in-time snapshot of host resource usage plus the
/// system context needed to make it legible. Linux-only for now (a
/// disclosed gap, not a silent assumption -- see /agent/README.md):
/// every host this has been deployed to so far is Linux, and a Windows
/// implementation (perf counters/WMI) is real future work, not
/// attempted here.
pub async fn collect(disk_path: &str) -> Result<Metrics> {
    let cpu_percent = cpu_percent().await.context("reading CPU usage")?;
    let (mem_used_bytes, mem_total_bytes) = memory().context("reading memory usage")?;
    let (disk_used_bytes, disk_total_bytes) = disk(disk_path).context("reading disk usage")?;
    // Soft-fail on all four: none of these should ever cost a whole
    // sample (losing real cpu_percent/mem/disk numbers) just because
    // e.g. /etc/os-release is missing on some minimal distro --
    // consistent with this codebase's existing "an optional feature's
    // failure must never take down the thing it's supplementing"
    // posture (see send_heartbeat/CheckIn's own graceful degradation).
    let cpu_cores = cpu_cores().unwrap_or(0);
    let os_name = os_name().unwrap_or_else(|_| "unknown".to_string());
    let kernel_version = kernel_version().unwrap_or_else(|_| "unknown".to_string());
    let uptime_seconds = uptime_seconds().unwrap_or(0);
    let (ipv4_addresses, ipv6_addresses) = ip_addresses().unwrap_or_default();
    Ok(Metrics {
        cpu_percent,
        mem_used_bytes,
        mem_total_bytes,
        disk_used_bytes,
        disk_total_bytes,
        cpu_cores,
        os_name,
        kernel_version,
        arch: std::env::consts::ARCH,
        uptime_seconds,
        ipv4_addresses,
        ipv6_addresses,
    })
}

/// Two `/proc/stat` samples ~200ms apart, delta-based -- the standard
/// technique every `top`-like tool uses, since a single snapshot of
/// cumulative jiffies-since-boot can't express a percentage on its own.
/// Self-contained (no state threaded through main.rs's `select!` loop)
/// at the cost of blocking this one `collect()` call for ~200ms once
/// per `metrics.interval` tick -- an acceptable trade against the
/// complexity of holding a previous-sample struct across ticks in an
/// already-busy loop, for a feature that only runs once a minute by
/// default.
async fn cpu_percent() -> Result<f64> {
    let (total1, idle1) = read_proc_stat()?;
    tokio::time::sleep(Duration::from_millis(200)).await;
    let (total2, idle2) = read_proc_stat()?;

    let total_delta = total2.saturating_sub(total1);
    let idle_delta = idle2.saturating_sub(idle1);
    if total_delta == 0 {
        return Ok(0.0);
    }
    Ok((1.0 - (idle_delta as f64 / total_delta as f64)) * 100.0)
}

fn read_proc_stat() -> Result<(u64, u64)> {
    let contents = fs::read_to_string("/proc/stat").context("reading /proc/stat")?;
    parse_proc_stat(&contents)
}

/// Parses `/proc/stat`'s leading "cpu " line: user nice system idle
/// iowait irq softirq steal guest guest_nice, all in USER_HZ jiffies
/// since boot. Returns (total, idle) -- idle here is idle+iowait,
/// matching what every standard CPU%-from-/proc/stat implementation
/// treats as "not busy" (iowait is a CPU waiting on I/O, not doing
/// work, even though the kernel's own `idle` field alone doesn't
/// include it).
fn parse_proc_stat(contents: &str) -> Result<(u64, u64)> {
    let line = contents
        .lines()
        .find(|l| l.starts_with("cpu "))
        .context("/proc/stat has no leading \"cpu \" line")?;
    let fields: Vec<u64> = line.split_whitespace().skip(1).filter_map(|f| f.parse().ok()).collect();
    if fields.len() < 4 {
        anyhow::bail!("unexpected /proc/stat format: {line:?}");
    }
    let idle = fields[3] + fields.get(4).copied().unwrap_or(0);
    let total: u64 = fields.iter().sum();
    Ok((total, idle))
}

fn memory() -> Result<(u64, u64)> {
    let contents = fs::read_to_string("/proc/meminfo").context("reading /proc/meminfo")?;
    parse_meminfo(&contents)
}

/// Parses `/proc/meminfo`'s MemTotal/MemAvailable (kB). MemAvailable
/// (not MemFree) is the kernel's own "how much could a new process
/// actually get" estimate, accounting for reclaimable caches/buffers --
/// what a human means by "memory used" far better than MemFree alone
/// (a system with most of RAM in disk cache but MemFree near zero is
/// not actually under memory pressure).
fn parse_meminfo(contents: &str) -> Result<(u64, u64)> {
    let mut total_kb = None;
    let mut available_kb = None;
    for line in contents.lines() {
        if let Some(v) = line.strip_prefix("MemTotal:") {
            total_kb = parse_meminfo_kb(v);
        } else if let Some(v) = line.strip_prefix("MemAvailable:") {
            available_kb = parse_meminfo_kb(v);
        }
    }
    let total_kb = total_kb.context("MemTotal not found in /proc/meminfo")?;
    let available_kb = available_kb.context("MemAvailable not found in /proc/meminfo")?;
    let used_kb = total_kb.saturating_sub(available_kb);
    Ok((used_kb * 1024, total_kb * 1024))
}

fn parse_meminfo_kb(s: &str) -> Option<u64> {
    s.trim().trim_end_matches("kB").trim().parse().ok()
}

fn disk(path: &str) -> Result<(u64, u64)> {
    let output = Command::new("df").arg("-B1").arg(path).output().context("running df")?;
    if !output.status.success() {
        anyhow::bail!("df exited with status {}: {}", output.status, String::from_utf8_lossy(&output.stderr));
    }
    parse_df_output(&String::from_utf8_lossy(&output.stdout))
}

/// Shells out to `df` rather than linking a statvfs binding -- same
/// "shell out to a boring, ubiquitous tool rather than add a dependency
/// or FFI binding" precedent `source/journald.rs` already sets for
/// `journalctl` (see /agent/README.md's "Why journalctl, not
/// libsystemd"). `-B1` requests byte-granularity output instead of the
/// default 1K-block units, so no unit conversion is needed here. Total
/// is `used + available`, not the raw block count `df` also reports --
/// some filesystems (ext4's default ~5% root reservation) hold back
/// blocks a normal process can never use, which would make a "percent
/// full" computed against the raw total look artificially low; `used +
/// available` matches what `df`'s own `Use%` column is computed
/// against.
fn parse_df_output(stdout: &str) -> Result<(u64, u64)> {
    let data_line = stdout.lines().nth(1).context("df produced no data line")?;
    let fields: Vec<&str> = data_line.split_whitespace().collect();
    // Filesystem, 1B-blocks, Used, Available, Use%, Mounted on
    if fields.len() < 4 {
        anyhow::bail!("unexpected df output: {data_line:?}");
    }
    let used: u64 = fields[2].parse().context("parsing df's Used column")?;
    let available: u64 = fields[3].parse().context("parsing df's Available column")?;
    Ok((used, used + available))
}

fn cpu_cores() -> Result<u32> {
    let contents = fs::read_to_string("/proc/cpuinfo").context("reading /proc/cpuinfo")?;
    parse_cpuinfo_core_count(&contents)
}

/// Counts `processor\t: N` lines in `/proc/cpuinfo` -- one per logical
/// CPU (a hyperthreaded core counts as two, same as what `nproc`/every
/// scheduler-facing tool means by "CPU count"), which is what
/// `cpu_percent`'s 0-100 scale is an average across.
fn parse_cpuinfo_core_count(contents: &str) -> Result<u32> {
    let n = contents.lines().filter(|l| l.starts_with("processor")).count() as u32;
    if n == 0 {
        anyhow::bail!("no \"processor\" lines found in /proc/cpuinfo");
    }
    Ok(n)
}

fn os_name() -> Result<String> {
    let contents = fs::read_to_string("/etc/os-release").context("reading /etc/os-release")?;
    parse_os_release_pretty_name(&contents)
}

/// Parses `/etc/os-release`'s `PRETTY_NAME="..."` line (e.g. "Debian
/// GNU/Linux 13 (trixie)") -- the one field every distro's os-release
/// is guaranteed to carry for exactly this "show a human a readable OS
/// name" purpose (see os-release(5)).
fn parse_os_release_pretty_name(contents: &str) -> Result<String> {
    contents
        .lines()
        .find_map(|l| l.strip_prefix("PRETTY_NAME="))
        .map(|v| v.trim().trim_matches('"').to_string())
        .context("PRETTY_NAME not found in /etc/os-release")
}

/// `/proc/sys/kernel/osrelease` is just the bare version string (e.g.
/// "6.12.90+deb13.1-amd64") with no parsing needed -- simpler and more
/// robust than picking the version back out of `/proc/version`'s
/// free-form `uname -a`-style sentence.
fn kernel_version() -> Result<String> {
    Ok(fs::read_to_string("/proc/sys/kernel/osrelease")
        .context("reading /proc/sys/kernel/osrelease")?
        .trim()
        .to_string())
}

fn uptime_seconds() -> Result<u64> {
    let contents = fs::read_to_string("/proc/uptime").context("reading /proc/uptime")?;
    parse_uptime(&contents)
}

/// `/proc/uptime`'s first field is seconds since boot (as a float, to
/// centisecond precision) -- the second field (total idle time summed
/// across all cores) isn't relevant here.
fn parse_uptime(contents: &str) -> Result<u64> {
    let first = contents.split_whitespace().next().context("/proc/uptime is empty")?;
    let seconds: f64 = first.parse().context("parsing /proc/uptime's first field")?;
    Ok(seconds as u64)
}

fn ip_addresses() -> Result<(Vec<String>, Vec<String>)> {
    let output = Command::new("ip").arg("-o").arg("addr").arg("show").output().context("running ip addr show")?;
    if !output.status.success() {
        anyhow::bail!("ip exited with status {}: {}", output.status, String::from_utf8_lossy(&output.stderr));
    }
    Ok(parse_ip_addr_output(&String::from_utf8_lossy(&output.stdout)))
}

/// Shells out to `ip -o addr show` -- same "boring, ubiquitous tool"
/// precedent `disk`'s `df` call and `source/journald.rs`'s `journalctl`
/// call already set, over an FFI binding to `getifaddrs(3)`. `-o`
/// (oneline) puts each address on its own line, e.g.:
///   2: eth0    inet 172.239.44.244/24 brd ... scope global eth0\ ...
///   2: eth0    inet6 fe80::1/64 scope link \ ...
/// Skips the loopback interface by name (`lo`) and any address whose
/// line mentions `scope link` (IPv6 link-local, `fe80::/10`) or
/// `scope host` (loopback addresses `ip` sometimes reports even on a
/// non-`lo` line) -- neither is what a viewer means by "this host's
/// IP". Field 1 is the interface name, field 2 is the address family
/// (`inet`/`inet6`), field 3 is `address/prefix-length`.
fn parse_ip_addr_output(stdout: &str) -> (Vec<String>, Vec<String>) {
    let mut v4 = Vec::new();
    let mut v6 = Vec::new();
    for line in stdout.lines() {
        let fields: Vec<&str> = line.split_whitespace().collect();
        if fields.len() < 4 {
            continue;
        }
        let iface = fields[1];
        if iface == "lo" || line.contains("scope link") || line.contains("scope host") {
            continue;
        }
        let addr = fields[3].split('/').next().unwrap_or(fields[3]);
        match fields[2] {
            "inet" => v4.push(addr.to_string()),
            "inet6" => v6.push(addr.to_string()),
            _ => {}
        }
    }
    v4.sort();
    v4.dedup();
    v6.sort();
    v6.dedup();
    (v4, v6)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_proc_stat() {
        let contents = "cpu  100 0 50 800 20 0 0 0 0 0\ncpu0 100 0 50 800 20 0 0 0 0 0\n";
        let (total, idle) = parse_proc_stat(contents).unwrap();
        // total = 100+0+50+800+20 = 970; idle = 800 (idle) + 20 (iowait) = 820
        assert_eq!(total, 970);
        assert_eq!(idle, 820);
    }

    #[test]
    fn rejects_proc_stat_with_no_cpu_line() {
        assert!(parse_proc_stat("not cpu data\n").is_err());
    }

    #[test]
    fn parses_meminfo() {
        let contents = "MemTotal:       16384000 kB\nMemFree:         1000000 kB\nMemAvailable:    8192000 kB\n";
        let (used, total) = parse_meminfo(contents).unwrap();
        assert_eq!(total, 16384000 * 1024);
        assert_eq!(used, (16384000 - 8192000) * 1024);
    }

    #[test]
    fn rejects_meminfo_missing_fields() {
        assert!(parse_meminfo("MemTotal: 16384000 kB\n").is_err());
    }

    #[test]
    fn parses_df_output() {
        let stdout = "Filesystem       1B-blocks       Used  Available Use% Mounted on\n/dev/sda1      80000000000 20000000000 60000000000  25% /\n";
        let (used, total) = parse_df_output(stdout).unwrap();
        assert_eq!(used, 20000000000);
        assert_eq!(total, 20000000000 + 60000000000);
    }

    #[test]
    fn rejects_df_output_with_no_data_line() {
        assert!(parse_df_output("Filesystem 1B-blocks Used Available Use% Mounted on\n").is_err());
    }

    #[test]
    fn counts_cpuinfo_processors() {
        let contents = "processor\t: 0\nmodel name\t: x\n\nprocessor\t: 1\nmodel name\t: x\n";
        assert_eq!(parse_cpuinfo_core_count(contents).unwrap(), 2);
    }

    #[test]
    fn rejects_cpuinfo_with_no_processor_lines() {
        assert!(parse_cpuinfo_core_count("model name: x\n").is_err());
    }

    #[test]
    fn parses_os_release_pretty_name() {
        let contents = "NAME=\"Debian GNU/Linux\"\nPRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nVERSION_ID=\"13\"\n";
        assert_eq!(parse_os_release_pretty_name(contents).unwrap(), "Debian GNU/Linux 13 (trixie)");
    }

    #[test]
    fn rejects_os_release_missing_pretty_name() {
        assert!(parse_os_release_pretty_name("NAME=\"Debian\"\n").is_err());
    }

    #[test]
    fn parses_uptime() {
        assert_eq!(parse_uptime("12345.67 98765.43\n").unwrap(), 12345);
    }

    #[test]
    fn rejects_empty_uptime() {
        assert!(parse_uptime("").is_err());
    }

    #[test]
    fn parses_ip_addr_output_excluding_loopback_and_link_local() {
        let stdout = concat!(
            "1: lo    inet 127.0.0.1/8 scope host lo\\       valid_lft forever preferred_lft forever\n",
            "1: lo    inet6 ::1/128 scope host \\       valid_lft forever preferred_lft forever\n",
            "2: eth0    inet 172.239.44.244/24 brd 172.239.44.255 scope global eth0\\       valid_lft forever preferred_lft forever\n",
            "2: eth0    inet6 2600:3c06::1/64 scope global dynamic mngtmpaddr noprefixroute \\       valid_lft forever preferred_lft forever\n",
            "2: eth0    inet6 fe80::abcd/64 scope link \\       valid_lft forever preferred_lft forever\n",
        );
        let (v4, v6) = parse_ip_addr_output(stdout);
        assert_eq!(v4, vec!["172.239.44.244".to_string()]);
        assert_eq!(v6, vec!["2600:3c06::1".to_string()]);
    }

    #[test]
    fn parses_ip_addr_output_dedupes_and_sorts_multiple_interfaces() {
        let stdout = concat!(
            "2: eth0    inet 10.0.0.5/24 scope global eth0\\       valid_lft forever preferred_lft forever\n",
            "3: eth1    inet 10.0.0.2/24 scope global eth1\\       valid_lft forever preferred_lft forever\n",
            "3: eth1    inet 10.0.0.5/24 scope global secondary eth1\\       valid_lft forever preferred_lft forever\n",
        );
        let (v4, _v6) = parse_ip_addr_output(stdout);
        assert_eq!(v4, vec!["10.0.0.2".to_string(), "10.0.0.5".to_string()]);
    }

    #[test]
    fn parses_ip_addr_output_with_no_addresses() {
        let (v4, v6) = parse_ip_addr_output("1: lo    inet 127.0.0.1/8 scope host lo\\       valid_lft forever preferred_lft forever\n");
        assert!(v4.is_empty());
        assert!(v6.is_empty());
    }
}
