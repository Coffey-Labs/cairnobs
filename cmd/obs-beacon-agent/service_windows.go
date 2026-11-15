package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// The Windows service (docs/windows.md). It runs as its own virtual
// account, NT SERVICE\obs-beacon-agent, which Windows makes with the
// service: no password, no administrator rights, nothing beyond a plain
// user's but its own data folder.
const (
	serviceName    = "obs-beacon-agent"
	serviceAccount = `NT SERVICE\` + serviceName
	serviceDisplay = "Cairn OBS agent"
	serviceAbout   = "Reports this host's health to Cairn OBS (obs-beacond)."
)

// installedExe is where `service install` puts the agent: under Program
// Files, which only administrators can change.
func installedExe() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "Cairn OBS", serviceName+".exe")
}

// serviceLogging sends the log to the Application event log when the
// Service Control Manager started the agent, and says whether it did.
func serviceLogging() bool {
	in, err := svc.IsWindowsService()
	if err != nil || !in {
		return false
	}
	if el, err := eventlog.Open(serviceName); err == nil {
		log.SetOutput(eventWriter{el})
	}
	return true
}

type eventWriter struct{ el *eventlog.Log }

func (w eventWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if strings.HasPrefix(msg, "obs-beacon-agent: ") { // log.Fatalf's
		_ = w.el.Error(1, msg)
	} else {
		_ = w.el.Info(1, msg)
	}
	return len(p), nil
}

func runAsService(cfg *config) error {
	return svc.Run(serviceName, &service{cfg: cfg})
}

type service struct{ cfg *config }

func (s *service) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, s.cfg) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(20 * time.Second):
				}
				return false, 0
			}
		case err := <-done:
			if err != nil {
				log.Printf("obs-beacon-agent: %v", err)
				return true, 1 // a service-specific exit code: the recovery actions restart it
			}
			return false, 0
		}
	}
}

// runService is `obs-beacon-agent service install|uninstall`, run as an
// administrator. install copies the agent under Program Files, registers
// the service (or, when it's there already, stops it, replaces the
// program and starts it again: an upgrade), makes the data folder and
// gives it to the service's account, and starts the service when the
// config is there.
func runService(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: obs-beacon-agent service install [-config path] | uninstall")
	}
	switch args[0] {
	case "install":
		fs := flag.NewFlagSet("service install", flag.ExitOnError)
		cfgPath := fs.String("config", defaultConfig, "the config file the service reads")
		_ = fs.Parse(args[1:])
		return installService(*cfgPath)
	case "uninstall":
		return uninstallService()
	}
	return fmt.Errorf("%q: install or uninstall", args[0])
}

func installService(cfgPath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("the Service Control Manager: %w (run as Administrator)", err)
	}
	defer m.Disconnect()
	exe := installedExe()
	s, err := m.OpenService(serviceName)
	upgrade := err == nil
	if upgrade {
		defer s.Close()
		if err := stopService(s); err != nil {
			return err
		}
	}
	if err := copySelf(exe); err != nil {
		return err
	}
	if !upgrade {
		s, err = m.CreateService(serviceName, exe, mgr.Config{
			DisplayName:      serviceDisplay,
			Description:      serviceAbout,
			StartType:        mgr.StartAutomatic,
			ServiceStartName: serviceAccount,
		}, "-config", cfgPath)
		if err != nil {
			return fmt.Errorf("creating the service: %w", err)
		}
		defer s.Close()
		restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 15 * time.Second}
		if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, 24*60*60); err != nil {
			return fmt.Errorf("setting restart on failure: %w", err)
		}
		if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
			return fmt.Errorf("setting restart on failure: %w", err)
		}
		// Already registered from an earlier install is fine.
		if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil && !strings.Contains(err.Error(), "exists") {
			return fmt.Errorf("registering with the event log: %w", err)
		}
	}
	if err := ownDataDir(filepath.Dir(cfgPath)); err != nil {
		return err
	}
	if _, err := os.Stat(cfgPath); err != nil {
		fmt.Printf("installed %s as the %q service; it starts once %s is there (write it, or run `%s join <token>`), then `Start-Service %s`\n",
			exe, serviceName, cfgPath, exe, serviceName)
		return nil
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("starting the service: %w", err)
	}
	if upgrade {
		fmt.Printf("upgraded %s and restarted the service\n", exe)
	} else {
		fmt.Printf("installed %s as the %q service, running as %s\n", exe, serviceName, serviceAccount)
	}
	return nil
}

// stopService stops it and waits, up to half a minute.
func stopService(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if st.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil {
			return fmt.Errorf("stopping the service: %w", err)
		}
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		if st, err = s.Query(); err != nil {
			return err
		}
		if st.State == svc.Stopped {
			return nil
		}
	}
	return errors.New("the service didn't stop within 30s")
}

// copySelf puts this program at dst, unless it's running from there.
func copySelf(dst string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	if strings.EqualFold(filepath.Clean(src), filepath.Clean(dst)) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst) // replaces it: the service is stopped
}

// ownDataDir makes the config and state folder and sets who may use it:
// SYSTEM and Administrators fully, the service's account to read and
// write, nobody else, and nothing inherited from ProgramData (where every
// user may read).
func ownDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sid, _, _, err := windows.LookupSID("", serviceAccount)
	if err != nil {
		return fmt.Errorf("the service's account: %w", err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1301bf;;;" + sid.String() + ")")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("%s: setting who may use it: %w", dir, err)
	}
	return nil
}

// uninstallService stops and removes the service and the program; the
// config and state stay, as a package's do.
func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("the Service Control Manager: %w (run as Administrator)", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("no %q service: %w", serviceName, err)
	}
	defer s.Close()
	if err := stopService(s); err != nil {
		return err
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("removing the service: %w", err)
	}
	_ = eventlog.Remove(serviceName)
	exe := installedExe()
	if self, _ := os.Executable(); !strings.EqualFold(filepath.Clean(self), filepath.Clean(exe)) {
		_ = os.Remove(exe)
		_ = os.Remove(filepath.Dir(exe))
	}
	fmt.Printf("removed the %q service; %s is kept\n", serviceName, dataDir)
	return nil
}
