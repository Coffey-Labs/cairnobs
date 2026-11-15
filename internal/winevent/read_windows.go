//go:build windows

package winevent

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/cairnobs/cairnobs/internal/report"
)

var (
	wevtapi                      = windows.NewLazySystemDLL("wevtapi.dll")
	procEvtQuery                 = wevtapi.NewProc("EvtQuery")
	procEvtNext                  = wevtapi.NewProc("EvtNext")
	procEvtRender                = wevtapi.NewProc("EvtRender")
	procEvtClose                 = wevtapi.NewProc("EvtClose")
	procEvtOpenPublisherMetadata = wevtapi.NewProc("EvtOpenPublisherMetadata")
	procEvtFormatMessage         = wevtapi.NewProc("EvtFormatMessage")
)

const (
	evtQueryChannelPath      = 0x1
	evtQueryReverseDirection = 0x200
	evtRenderEventXML        = 1
	evtFormatMessageEvent    = 1
	// errEvtUnresolvedValueInsert: the message came back with an insert
	// Windows couldn't fill; it's still the message.
	errEvtUnresolvedValueInsert = windows.Errno(15029)
)

// Read is the errors and criticals in System and Application over the
// last day, grouped. A log it can't read is named in Error; what the
// others had still comes back.
func Read(now time.Time) *report.Events {
	var all []Event
	var errs []string
	for _, log := range Logs {
		ev, err := readLog(log, Window)
		if err != nil {
			errs = append(errs, log+": "+err.Error())
		}
		all = append(all, ev...)
	}
	return &report.Events{Since: now.Add(-Window).UTC(), List: Group(all), Error: strings.Join(errs, "; ")}
}

func readLog(log string, window time.Duration) ([]Event, error) {
	path, err := windows.UTF16PtrFromString(log)
	if err != nil {
		return nil, err
	}
	query, _ := windows.UTF16PtrFromString(fmt.Sprintf("*[System[(Level=1 or Level=2) and TimeCreated[timediff(@SystemTime) <= %d]]]", window.Milliseconds()))
	h, _, err := procEvtQuery.Call(0, uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(query)), evtQueryChannelPath|evtQueryReverseDirection)
	if h == 0 {
		return nil, err
	}
	defer evtClose(h)
	pubs := map[string]uintptr{} // the sources' message tables, opened once
	defer func() {
		for _, p := range pubs {
			if p != 0 {
				evtClose(p)
			}
		}
	}()
	type key struct {
		source string
		id     uint32
	}
	seen := map[key]bool{} // newest first: only a group's first event needs its message
	var out []Event
	handles := make([]uintptr, 64)
	for len(out) < PerLog {
		var n uint32
		r, _, err := procEvtNext.Call(h, uintptr(len(handles)), uintptr(unsafe.Pointer(&handles[0])), 10_000, 0, uintptr(unsafe.Pointer(&n)))
		if r == 0 {
			if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
				break
			}
			return out, err
		}
		for _, e := range handles[:n] {
			if b, err := render(e); err == nil {
				if ev, ok := ParseXML(log, b); ok {
					if k := (key{ev.Source, ev.ID}); !seen[k] {
						seen[k] = true
						ev.Message = message(pubs, ev.Source, e)
					}
					out = append(out, ev)
				}
			}
			evtClose(e)
		}
	}
	return out, nil
}

// render is an event's XML.
func render(e uintptr) ([]byte, error) {
	var used, props uint32
	r, _, err := procEvtRender.Call(0, e, evtRenderEventXML, 0, 0, uintptr(unsafe.Pointer(&used)), uintptr(unsafe.Pointer(&props)))
	if r == 0 && !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		return nil, err
	}
	buf := make([]uint16, used/2+1)
	r, _, err = procEvtRender.Call(0, e, evtRenderEventXML, uintptr(len(buf)*2), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&used)), uintptr(unsafe.Pointer(&props)))
	if r == 0 {
		return nil, err
	}
	return []byte(windows.UTF16ToString(buf)), nil
}

// message is the event's text from its source's message table, or "".
func message(pubs map[string]uintptr, source string, e uintptr) string {
	p, ok := pubs[source]
	if !ok {
		name, _ := windows.UTF16PtrFromString(source)
		p, _, _ = procEvtOpenPublisherMetadata.Call(0, uintptr(unsafe.Pointer(name)), 0, 0, 0)
		pubs[source] = p
	}
	if p == 0 {
		return ""
	}
	var used uint32
	r, _, err := procEvtFormatMessage.Call(p, e, 0, 0, 0, evtFormatMessageEvent, 0, 0, uintptr(unsafe.Pointer(&used)))
	if r == 0 && !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || used == 0 {
		return ""
	}
	buf := make([]uint16, used+1)
	r, _, err = procEvtFormatMessage.Call(p, e, 0, 0, 0, evtFormatMessageEvent, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&used)))
	if r == 0 && !errors.Is(err, errEvtUnresolvedValueInsert) {
		return ""
	}
	return windows.UTF16ToString(buf)
}

func evtClose(h uintptr) { _, _, _ = procEvtClose.Call(h) }
