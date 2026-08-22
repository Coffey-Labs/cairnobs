//! Windows Service Control Manager integration: install/uninstall the
//! agent as a native Windows service, and the SCM-invoked entry point
//! that actually runs it as one.
//!
//! UNVERIFIED, same caveat as source/windows_eventlog.rs and
//! source/etw.rs -- written against the `windows-service` crate's
//! documented usage pattern, not compiled or run (no Windows toolchain
//! available). This one is lower-risk than etw.rs (no raw FFI struct
//! layout to get right; `windows-service` wraps that), but the
//! stop-signal plumbing between the SCM callback and the tokio-running
//! agent thread is new code worth testing carefully.
//!
//! Known limitation, not addressed here: when running as a service (no
//! console attached), `tracing_subscriber::fmt()`'s stdout writer has
//! nowhere to go. Logs won't be visible anywhere useful until this is
//! redirected to a file or an actual Windows Event Log tracing sink is
//! written -- flagging this now rather than shipping it silently broken.

use anyhow::{Context, Result};
use std::ffi::OsString;
use std::time::Duration;
use windows_service::service::{
    ServiceAccess, ServiceControl, ServiceControlAccept, ServiceErrorControl, ServiceExitCode,
    ServiceInfo, ServiceStartType, ServiceState, ServiceStatus, ServiceType,
};
use windows_service::service_control_handler::{self, ServiceControlHandlerResult};
use windows_service::service_manager::{ServiceManager, ServiceManagerAccess};
use windows_service::{define_windows_service, service_dispatcher};

pub const SERVICE_NAME: &str = "CairnObsAgent";
const SERVICE_TYPE: ServiceType = ServiceType::OWN_PROCESS;

/// Registers this binary as a Windows service: Automatic start,
/// LocalSystem account, invoked with the `run-service` subcommand (which
/// is what the SCM actually launches — not a bare `cairnobs-agent` with no
/// arguments). Requires an administrator shell.
pub fn install() -> Result<()> {
    let manager = ServiceManager::local_computer(None::<&str>, ServiceManagerAccess::CREATE_SERVICE)
        .context("opening Service Control Manager")?;

    let exe_path = std::env::current_exe().context("resolving current executable path")?;

    let service_info = ServiceInfo {
        name: OsString::from(SERVICE_NAME),
        display_name: OsString::from("Cairn OBS Log Agent"),
        service_type: SERVICE_TYPE,
        start_type: ServiceStartType::AutoStart,
        error_control: ServiceErrorControl::Normal,
        executable_path: exe_path,
        launch_arguments: vec![OsString::from("run-service")],
        dependencies: vec![],
        account_name: None, // LocalSystem
        account_password: None,
    };

    let service = manager
        .create_service(&service_info, ServiceAccess::CHANGE_CONFIG)
        .context("creating service")?;
    service
        .set_description("Ships local logs to Cairn OBS ingest over mTLS.")
        .context("setting service description")?;

    tracing::info!(service = SERVICE_NAME, "installed Windows service");
    Ok(())
}

/// Removes the service registration. Does not stop a currently-running
/// instance first — stop it via `services.msc`/`sc.exe stop` before
/// uninstalling if it's running.
pub fn uninstall() -> Result<()> {
    let manager = ServiceManager::local_computer(None::<&str>, ServiceManagerAccess::CONNECT)
        .context("opening Service Control Manager")?;
    let service = manager
        .open_service(SERVICE_NAME, ServiceAccess::DELETE)
        .context("opening service for deletion")?;
    service.delete().context("deleting service")?;

    tracing::info!(service = SERVICE_NAME, "removed Windows service");
    Ok(())
}

define_windows_service!(ffi_service_main, service_main);

/// Blocks, handing control to the SCM dispatch loop -- this is what
/// `main()` calls for the `run-service` subcommand, which is what the SCM
/// itself launches when the service starts. Must not be called from
/// inside a tokio runtime (see the doc comment on `main()` in main.rs).
pub fn run_as_service() -> Result<()> {
    service_dispatcher::start(SERVICE_NAME, ffi_service_main)
        .context("starting Windows service dispatcher")
}

fn service_main(_arguments: Vec<OsString>) {
    if let Err(e) = run_service() {
        // Nowhere better to put this yet -- see the module-level caveat
        // about tracing having no attached console under the SCM.
        tracing::error!(error = ?e, "windows service run failed");
    }
}

fn run_service() -> Result<()> {
    let (shutdown_tx, shutdown_rx) = std::sync::mpsc::channel::<()>();

    let event_handler = move |control_event| -> ServiceControlHandlerResult {
        match control_event {
            ServiceControl::Interrogate => ServiceControlHandlerResult::NoError,
            ServiceControl::Stop => {
                let _ = shutdown_tx.send(());
                ServiceControlHandlerResult::NoError
            }
            _ => ServiceControlHandlerResult::NotImplemented,
        }
    };

    let status_handle = service_control_handler::register(SERVICE_NAME, event_handler)
        .context("registering service control handler")?;

    status_handle
        .set_service_status(ServiceStatus {
            service_type: SERVICE_TYPE,
            current_state: ServiceState::Running,
            controls_accepted: ServiceControlAccept::STOP,
            exit_code: ServiceExitCode::Win32(0),
            checkpoint: 0,
            wait_hint: Duration::default(),
            process_id: None,
        })
        .context("reporting Running status to the SCM")?;

    // service_main is invoked by the SCM on a plain thread, not an async
    // context -- build a dedicated tokio runtime here and run the actual
    // agent on it, same `run_agent` entry point a normal foreground run
    // uses. Block this thread until either the agent exits on its own or
    // the SCM asks us to stop.
    let agent_thread = std::thread::spawn(|| {
        let rt = match tokio::runtime::Runtime::new() {
            Ok(rt) => rt,
            Err(e) => {
                tracing::error!(error = %e, "building tokio runtime for service run");
                return;
            }
        };
        if let Err(e) = rt.block_on(crate::run_agent(None)) {
            tracing::error!(error = %e, "agent exited with error while running as a service");
        }
    });

    loop {
        if shutdown_rx.recv_timeout(Duration::from_millis(500)).is_ok() {
            break;
        }
        if agent_thread.is_finished() {
            break;
        }
    }

    status_handle
        .set_service_status(ServiceStatus {
            service_type: SERVICE_TYPE,
            current_state: ServiceState::Stopped,
            controls_accepted: ServiceControlAccept::empty(),
            exit_code: ServiceExitCode::Win32(0),
            checkpoint: 0,
            wait_hint: Duration::default(),
            process_id: None,
        })
        .context("reporting Stopped status to the SCM")?;

    Ok(())
}
