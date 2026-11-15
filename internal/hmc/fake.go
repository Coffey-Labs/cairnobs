package hmc

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Fake is an HMC's REST API as IBM's own captured answers show it (an Atom
// feed per list, each object under its namespace, the session in a
// LogonResponse), for tests: the servers and partitions in Servers, the
// user and password in User and Password. Change Servers between reads to
// make trouble; Expire drops every session, as an HMC's idle timeout does.
type Fake struct {
	User, Password string
	Console        string

	mu       sync.Mutex
	Servers  []Server
	sessions map[string]bool
	n        int
	Calls    []string // "PUT /rest/api/web/Logon", "GET /rest/api/uom/ManagedSystem?group=None", …
	Logons   int
}

// Expire drops every session.
func (f *Fake) Expire() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = nil
}

const (
	uomNS = "http://www.ibm.com/xmlns/systems/power/firmware/uom/mc/2012_10/"
	webNS = "http://www.ibm.com/xmlns/systems/power/firmware/web/mc/2012_10/"
)

// Handler serves the API.
func (f *Fake) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.Calls = append(f.Calls, r.Method+" "+r.URL.RequestURI())
		if r.URL.Path == "/rest/api/web/Logon" {
			f.logon(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}
		if !f.sessions[r.Header.Get("X-API-Session")] {
			f.httpError(w, http.StatusUnauthorized, "The session is not valid.")
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/rest/api/uom/")
		parts := strings.Split(path, "/")
		var b bytes.Buffer
		b.WriteString(`<feed xmlns="http://www.w3.org/2005/Atom" xmlns:ns2="http://a9.com/-/spec/opensearch/1.1/">`)
		switch {
		case path == "ManagementConsole":
			entry(&b, "ManagementConsole", uomNS, func(b *bytes.Buffer) { el(b, "ManagementConsoleName", f.Console) })
		case path == "ManagedSystem":
			for _, sv := range f.Servers {
				entry(&b, "ManagedSystem", uomNS, func(b *bytes.Buffer) { writeServer(b, sv) })
			}
		case len(parts) == 3 && parts[0] == "ManagedSystem" && (parts[2] == "LogicalPartition" || parts[2] == "VirtualIOServer"):
			var sv *Server
			for i := range f.Servers {
				if f.Servers[i].UUID == parts[1] {
					sv = &f.Servers[i]
				}
			}
			if sv == nil {
				f.httpError(w, http.StatusNotFound, "No such managed system.")
				return
			}
			if !strings.EqualFold(sv.State, "operating") && !strings.EqualFold(sv.State, "standby") && !strings.EqualFold(sv.State, "power off") {
				f.httpError(w, http.StatusInternalServerError, "The managed system is not in a valid state.")
				return
			}
			n := 0
			for _, p := range sv.Partitions {
				if p.VIOS() == (parts[2] == "VirtualIOServer") {
					entry(&b, parts[2], uomNS, func(b *bytes.Buffer) { writePartition(b, p) })
					n++
				}
			}
			if n == 0 {
				w.WriteHeader(http.StatusNoContent) // a feed with nothing in it
				return
			}
		default:
			f.httpError(w, http.StatusNotFound, "Unknown resource.")
			return
		}
		b.WriteString(`</feed>`)
		w.Header().Set("Content-Type", "application/atom+xml")
		w.Write(b.Bytes()) //nolint:errcheck // a test server
	})
}

func (f *Fake) logon(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		delete(f.sessions, r.Header.Get("X-API-Session"))
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPut:
	default:
		http.Error(w, "no", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		User string `xml:"UserID"`
		Pass string `xml:"Password"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if xml.Unmarshal(body, &req) != nil || req.User != f.User || req.Pass != f.Password ||
		!strings.HasPrefix(r.Header.Get("Content-Type"), webXML+"; type=LogonRequest") {
		f.httpError(w, http.StatusUnauthorized, "User name or password is not valid.")
		return
	}
	f.n++
	f.Logons++
	tok := fmt.Sprintf("session-%d", f.n)
	if f.sessions == nil {
		f.sessions = map[string]bool{}
	}
	f.sessions[tok] = true
	w.Header().Set("Content-Type", webXML+"; type=LogonResponse")
	fmt.Fprintf(w, `<LogonResponse xmlns=%q schemaVersion="V1_1_0"><Metadata><Atom/></Metadata><X-API-Session kb="ROR" kxe="false">%s</X-API-Session></LogonResponse>`, webNS, tok)
}

func (f *Fake) httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/atom+xml")
	w.WriteHeader(code)
	var b bytes.Buffer
	b.WriteString(`<entry xmlns="http://www.w3.org/2005/Atom"><title>HttpErrorResponse</title><content type="application/vnd.ibm.powervm.web+xml; type=HttpErrorResponse">`)
	fmt.Fprintf(&b, `<HttpErrorResponse:HttpErrorResponse xmlns:HttpErrorResponse=%q xmlns=%q schemaVersion="V1_2_0"><HTTPStatus>%d</HTTPStatus>`, webNS, webNS, code)
	el(&b, "Message", msg)
	b.WriteString(`</HttpErrorResponse:HttpErrorResponse></content></entry>`)
	w.Write(b.Bytes()) //nolint:errcheck // a test server
}

func entry(b *bytes.Buffer, kind, ns string, body func(*bytes.Buffer)) {
	fmt.Fprintf(b, `<entry><title>%s</title><content type="application/vnd.ibm.powervm.uom+xml; type=%s">`, kind, kind)
	fmt.Fprintf(b, `<%s:%s xmlns:%s=%q xmlns=%q schemaVersion="V1_0">`, kind, kind, kind, ns, ns)
	body(b)
	fmt.Fprintf(b, `</%s:%s></content></entry>`, kind, kind)
}

func el(b *bytes.Buffer, name string, v any) {
	fmt.Fprintf(b, `<%s kb="ROR" kxe="false">`, name)
	xml.EscapeText(b, []byte(fmt.Sprint(v))) //nolint:errcheck // a bytes.Buffer
	fmt.Fprintf(b, `</%s>`, name)
}

func writeServer(b *bytes.Buffer, sv Server) {
	fmt.Fprintf(b, `<Metadata><Atom><AtomID>%s</AtomID></Atom></Metadata>`, sv.UUID)
	b.WriteString(`<AssociatedSystemMemoryConfiguration schemaVersion="V1_0">`)
	el(b, "CurrentAvailableSystemMemory", sv.MemFreeMB)
	el(b, "InstalledSystemMemory", sv.MemMB)
	b.WriteString(`</AssociatedSystemMemoryConfiguration><AssociatedSystemProcessorConfiguration schemaVersion="V1_0">`)
	el(b, "CurrentAvailableSystemProcessorUnits", sv.ProcFree)
	el(b, "InstalledSystemProcessorUnits", sv.ProcUnits)
	b.WriteString(`</AssociatedSystemProcessorConfiguration>`)
	el(b, "DetailedState", sv.DetailedState)
	b.WriteString(`<MachineTypeModelAndSerialNumber schemaVersion="V1_0">`)
	el(b, "MachineType", sv.MachineType)
	el(b, "Model", sv.Model)
	el(b, "SerialNumber", sv.Serial)
	b.WriteString(`</MachineTypeModelAndSerialNumber>`)
	el(b, "PhysicalSystemAttentionLEDState", sv.AttentionLED)
	el(b, "State", sv.State)
	el(b, "SystemName", sv.Name)
}

func writePartition(b *bytes.Buffer, p Partition) {
	el(b, "IsVirtualServiceAttentionLEDOn", p.Attention)
	el(b, "OperatingSystemVersion", p.OS)
	el(b, "PartitionID", p.ID)
	b.WriteString(`<PartitionMemoryConfiguration schemaVersion="V1_0">`)
	el(b, "CurrentMemory", p.MemMB)
	b.WriteString(`</PartitionMemoryConfiguration>`)
	el(b, "PartitionName", p.Name)
	b.WriteString(`<PartitionProcessorConfiguration schemaVersion="V1_0">`)
	el(b, "CurrentHasDedicatedProcessors", p.Dedicated)
	el(b, "CurrentSharingMode", p.SharedMode)
	if p.Dedicated == "true" {
		b.WriteString(`<CurrentDedicatedProcessorConfiguration schemaVersion="V1_2_0">`)
		el(b, "CurrentProcessors", p.DedProcs)
		b.WriteString(`</CurrentDedicatedProcessorConfiguration>`)
	} else {
		b.WriteString(`<CurrentSharedProcessorConfiguration schemaVersion="V1_0">`)
		el(b, "AllocatedVirtualProcessors", p.VirtProcs)
		el(b, "CurrentProcessingUnits", p.ProcUnits)
		b.WriteString(`</CurrentSharedProcessorConfiguration>`)
	}
	b.WriteString(`</PartitionProcessorConfiguration>`)
	el(b, "PartitionState", p.State)
	el(b, "PartitionType", p.Type)
	el(b, "PartitionUUID", p.UUID)
	el(b, "ResourceMonitoringControlState", p.RMC)
	el(b, "ReferenceCode", p.RefCode)
}

// NewFakeServer is a healthy Power10 server with a pair of VIOS, an IBM i
// partition and an AIX one, all running.
func NewFakeServer(name, serial string, n int) Server {
	uuid := func(k int) string { return fmt.Sprintf("%08X-0000-4000-8000-%012X", n, k) }
	return Server{UUID: strings.ToLower(uuid(0)), Name: name, State: "operating", DetailedState: "None", MachineType: "9105", Model: "42A",
		Serial: serial, ProcUnits: 20, ProcFree: 4.5, MemMB: 1 << 20, MemFreeMB: 196_608, AttentionLED: "false",
		Partitions: []Partition{
			{UUID: uuid(1), ID: 1, Name: name + "-vios1", State: "running", Type: "Virtual IO Server", OS: "VIOS 4.1.1.00", RMC: "active",
				Attention: "false", MemMB: 16_384, Dedicated: "false", ProcUnits: 1, VirtProcs: 2, SharedMode: "uncapped", RefCode: ""},
			{UUID: uuid(2), ID: 2, Name: name + "-vios2", State: "running", Type: "Virtual IO Server", OS: "VIOS 4.1.1.00", RMC: "active",
				Attention: "false", MemMB: 16_384, Dedicated: "false", ProcUnits: 1, VirtProcs: 2, SharedMode: "uncapped"},
			{UUID: uuid(3), ID: 3, Name: "PROD1", State: "running", Type: "OS400", OS: "IBM i Licensed Internal Code 7.5", RMC: "none",
				Attention: "false", MemMB: 262_144, Dedicated: "true", DedProcs: 4, SharedMode: "keep_idle_procs", RefCode: "00000000"},
			{UUID: uuid(4), ID: 4, Name: "aixdb1", State: "running", Type: "AIX/Linux", OS: "AIX 7.3 7300-03-00-2446", RMC: "active",
				Attention: "false", MemMB: 131_072, Dedicated: "false", ProcUnits: 2.5, VirtProcs: 4, SharedMode: "uncapped"},
		}}
}
