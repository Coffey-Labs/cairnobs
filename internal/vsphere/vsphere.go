// Package vsphere reads a vCenter's VMs, ESXi hosts, datastores and
// triggered alarms (docs/cluster-watcher.md, step 8), as a user holding
// vCenter's built-in Read-only role: it can look, never change. It uses
// govmomi, VMware's Go SDK, and only reads properties.
package vsphere

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// Config is the agent's vsphere: section.
type Config struct {
	// Name is what the vCenter is kept under, beside the agent's name
	// ("mgmt1:vcsa-01.example.org"): default its host name.
	Name         string `yaml:"name"`
	URL          string `yaml:"url"`           // https://vcenter.example.org (the /sdk path is added)
	Username     string `yaml:"username"`      // a user with the Read-only role on the vCenter
	PasswordFile string `yaml:"password_file"` // its password, alone in the file
	CAFile       string `yaml:"ca_file"`       // the vCenter's CA (its VMCA root), PEM; else the system's
	// Fingerprint pins the vCenter's certificate (SHA-256, AA:BB:… as
	// vCenter shows it) when it isn't signed by a CA the agent trusts.
	Fingerprint string        `yaml:"fingerprint"`
	Every       time.Duration `yaml:"every"` // between reads: 60s
}

// Defaults fills in what's left out.
func (c *Config) Defaults() {
	if c.Every == 0 {
		c.Every = time.Minute
	}
	if c.Name == "" {
		if u, err := url.Parse(c.URL); err == nil {
			c.Name = strings.ToLower(u.Hostname())
		}
	}
}

// Client is one logged-in session, made again when it lapses.
type Client struct {
	cfg  Config
	u    *url.URL
	pass string
	c    *vim25.Client
}

// New checks cfg and reads the password; it doesn't connect yet.
func New(cfg Config) (*Client, error) {
	cfg.Defaults()
	u, err := url.Parse(strings.TrimRight(cfg.URL, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("vsphere: url %q: want https://vcenter", cfg.URL)
	}
	if u.Path == "" {
		u.Path = "/sdk"
	}
	if cfg.Username == "" {
		return nil, errors.New("vsphere: no username")
	}
	b, err := os.ReadFile(cfg.PasswordFile)
	if err != nil {
		return nil, fmt.Errorf("vsphere: the password: %w", err)
	}
	return &Client{cfg: cfg, u: u, pass: strings.TrimRight(string(b), "\r\n")}, nil
}

// Name is what the vCenter is called here: its host name.
func (c *Client) Name() string { return c.u.Hostname() }

func (c *Client) connect(ctx context.Context) error {
	sc := soap.NewClient(c.u, false)
	if c.cfg.CAFile != "" {
		if err := sc.SetRootCAs(c.cfg.CAFile); err != nil {
			return fmt.Errorf("vsphere: ca_file: %w", err)
		}
	}
	if c.cfg.Fingerprint != "" {
		sc.SetThumbprint(c.u.Host, strings.ToUpper(c.cfg.Fingerprint))
	}
	vc, err := vim25.NewClient(ctx, sc)
	if err != nil {
		return err
	}
	if err := session.NewManager(vc).Login(ctx, url.UserPassword(c.cfg.Username, c.pass)); err != nil {
		return err
	}
	c.c = vc
	return nil
}

// Close logs out.
func (c *Client) Close(ctx context.Context) {
	if c.c != nil {
		_ = session.NewManager(c.c).Logout(ctx)
		c.c = nil
	}
}

// Snapshot is one read of the vCenter.
type Snapshot struct {
	Name     string // the vCenter's host name
	Version  string // "8.0.3"
	Product  string // "VMware vCenter Server"
	VMs      []mo.VirtualMachine
	Hosts    []mo.HostSystem
	Stores   []mo.Datastore
	Alarms   map[string]string // alarm reference → its name
	Problems map[string]string
	At       time.Time
}

var (
	vmProps = []string{"name", "config.uuid", "config.template", "config.hardware.numCPU", "config.hardware.memoryMB",
		"config.guestFullName", "runtime.powerState", "runtime.connectionState", "runtime.host", "guest.toolsRunningStatus",
		"summary.quickStats.overallCpuUsage", "summary.quickStats.hostMemoryUsage", "snapshot.rootSnapshotList",
		"triggeredAlarmState", "parent", "resourcePool"}
	hostProps = []string{"name", "runtime.connectionState", "runtime.inMaintenanceMode", "runtime.powerState",
		"summary.hardware.numCpuCores", "summary.hardware.cpuMhz", "summary.hardware.memorySize", "summary.config.product.fullName",
		"summary.quickStats.overallCpuUsage", "summary.quickStats.overallMemoryUsage", "triggeredAlarmState", "parent"}
	storeProps = []string{"name", "summary.capacity", "summary.freeSpace", "summary.accessible", "summary.type", "triggeredAlarmState"}
)

// Read reads everything once, logging in first when there's no session
// (or it lapsed).
func (c *Client) Read(ctx context.Context, now time.Time) (*Snapshot, error) {
	s, err := c.read(ctx, now)
	if err != nil && c.c != nil && isAuth(err) {
		c.c = nil // the session lapsed: log in again, once
		s, err = c.read(ctx, now)
	}
	return s, err
}

func isAuth(err error) bool {
	if soap.IsSoapFault(err) {
		_, ok := soap.ToSoapFault(err).VimFault().(types.NotAuthenticated)
		return ok
	}
	return strings.Contains(err.Error(), "NotAuthenticated")
}

func (c *Client) read(ctx context.Context, now time.Time) (*Snapshot, error) {
	if c.c == nil {
		if err := c.connect(ctx); err != nil {
			return nil, err
		}
	}
	about := c.c.ServiceContent.About
	s := &Snapshot{Name: c.Name(), Version: about.Version, Product: about.Name, Alarms: map[string]string{}, Problems: map[string]string{}, At: now}
	m := view.NewManager(c.c)
	v, err := m.CreateContainerView(ctx, c.c.ServiceContent.RootFolder, []string{"VirtualMachine", "HostSystem", "Datastore"}, true)
	if err != nil {
		return nil, err
	}
	defer v.Destroy(context.Background()) //nolint:errcheck // the session ends it anyway
	if err := v.Retrieve(ctx, []string{"VirtualMachine"}, vmProps, &s.VMs); err != nil {
		return nil, fmt.Errorf("vsphere: VMs: %w", err)
	}
	if err := v.Retrieve(ctx, []string{"HostSystem"}, hostProps, &s.Hosts); err != nil {
		s.Problems["hosts"] = err.Error()
	}
	if err := v.Retrieve(ctx, []string{"Datastore"}, storeProps, &s.Stores); err != nil {
		s.Problems["datastores"] = err.Error()
	}
	// The triggered alarms' names, in one more call.
	refs := map[types.ManagedObjectReference]bool{}
	add := func(as []types.AlarmState) {
		for _, a := range as {
			refs[a.Alarm] = true
		}
	}
	for _, x := range s.VMs {
		add(x.TriggeredAlarmState)
	}
	for _, x := range s.Hosts {
		add(x.TriggeredAlarmState)
	}
	for _, x := range s.Stores {
		add(x.TriggeredAlarmState)
	}
	if len(refs) > 0 {
		list := make([]types.ManagedObjectReference, 0, len(refs))
		for r := range refs {
			list = append(list, r)
		}
		var alarms []mo.Alarm
		if err := property.DefaultCollector(c.c).Retrieve(ctx, list, []string{"info.name"}, &alarms); err != nil {
			s.Problems["alarms"] = err.Error()
		}
		for _, a := range alarms {
			s.Alarms[a.Self.Value] = a.Info.Name
		}
	}
	return s, nil
}
