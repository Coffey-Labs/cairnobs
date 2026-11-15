package units

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/cairnobs/cairnobs/internal/report"
)

// openSCM asks the Service Control Manager only for what any user may:
// to connect and to list services.
func openSCM() (windows.Handle, error) {
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return 0, fmt.Errorf("the Service Control Manager: %w", err)
	}
	return h, nil
}

// Read asks the Service Control Manager about the named services.
func Read(_ context.Context, names []string) ([]report.Unit, error) {
	if len(names) == 0 {
		return nil, nil
	}
	scm, err := openSCM()
	if err != nil {
		return nil, err
	}
	defer windows.CloseServiceHandle(scm)
	out := make([]report.Unit, 0, len(names))
	var errs []error
	for _, n := range names {
		unit := Name(n)
		u, err := readService(scm, unit)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ServiceName(unit), err))
			continue
		}
		out = append(out, u)
	}
	return out, errors.Join(errs...)
}

func readService(scm windows.Handle, unit string) (report.Unit, error) {
	name, err := windows.UTF16PtrFromString(ServiceName(unit))
	if err != nil {
		return report.Unit{}, err
	}
	h, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return notFound(unit), nil
	}
	if err != nil {
		return report.Unit{}, err
	}
	defer windows.CloseServiceHandle(h)
	var st windows.SERVICE_STATUS_PROCESS
	var need uint32
	if err := windows.QueryServiceStatusEx(h, windows.SC_STATUS_PROCESS_INFO, (*byte)(unsafe.Pointer(&st)), uint32(unsafe.Sizeof(st)), &need); err != nil {
		return report.Unit{}, err
	}
	startType, err := startTypeOf(h)
	if err != nil {
		return report.Unit{}, err
	}
	exit := st.Win32ExitCode
	if exit == uint32(windows.ERROR_SERVICE_SPECIFIC_ERROR) {
		exit = max(st.ServiceSpecificExitCode, 1)
	}
	return scmUnit(unit, st.CurrentState, startType, exit, processStart(st.ProcessId)), nil
}

func startTypeOf(h windows.Handle) (uint32, error) {
	var need uint32
	err := windows.QueryServiceConfig(h, nil, 0, &need)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		return 0, err
	}
	buf := make([]byte, need)
	cfg := (*windows.QUERY_SERVICE_CONFIG)(unsafe.Pointer(&buf[0]))
	if err := windows.QueryServiceConfig(h, cfg, need, &need); err != nil {
		return 0, err
	}
	return cfg.StartType, nil
}

// processStart is when the service's process started, for "running since":
// zero when it can't be told (a shared svchost started with the system, or
// a process this account may not look at).
func processStart(pid uint32) time.Time {
	if pid == 0 {
		return time.Time{}
	}
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return time.Time{}
	}
	defer windows.CloseHandle(p)
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(p, &created, &exited, &kernel, &user) != nil {
		return time.Time{}
	}
	return time.Unix(0, created.Nanoseconds()).UTC().Truncate(time.Second)
}

// Running lists the services running now, as units ("W32Time.service"),
// leaving out the per-user instances that come and go with logins.
func Running(context.Context) ([]string, error) {
	scm, err := openSCM()
	if err != nil {
		return nil, err
	}
	defer windows.CloseServiceHandle(scm)
	var need, count, resume uint32
	buf := make([]byte, 64<<10)
	var names []string
	for {
		err := windows.EnumServicesStatusEx(scm, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32, windows.SERVICE_ACTIVE,
			&buf[0], uint32(len(buf)), &need, &count, &resume, nil)
		if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return nil, fmt.Errorf("listing services: %w", err)
		}
		if count > 0 {
			list := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buf[0])), count)
			for _, s := range list {
				const perUser = 0x40 | 0x80 // SERVICE_USER_SERVICE, SERVICE_USERSERVICE_INSTANCE
				if s.ServiceStatusProcess.ServiceType&perUser != 0 || s.ServiceStatusProcess.CurrentState != scmRunning {
					continue
				}
				names = append(names, Name(windows.UTF16PtrToString(s.ServiceName)))
			}
		}
		if err == nil {
			break
		}
		if int(need) > len(buf) {
			buf = make([]byte, need)
		}
	}
	slices.Sort(names)
	return names, nil
}
