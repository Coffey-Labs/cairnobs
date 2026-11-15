//go:build windows

package hyperv

import (
	"fmt"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A small WMI client over COM, without cgo or a COM library: connect to a
// namespace, run a WQL query, read properties. Only what this package
// asks for: strings, integers and booleans. Call it on one locked OS
// thread, between coInit and coUninit.

var (
	ole32                    = windows.NewLazySystemDLL("ole32.dll")
	oleaut32                 = windows.NewLazySystemDLL("oleaut32.dll")
	procCoInitializeEx       = ole32.NewProc("CoInitializeEx")
	procCoUninitialize       = ole32.NewProc("CoUninitialize")
	procCoInitializeSecurity = ole32.NewProc("CoInitializeSecurity")
	procCoCreateInstance     = ole32.NewProc("CoCreateInstance")
	procCoSetProxyBlanket    = ole32.NewProc("CoSetProxyBlanket")
	procSysAllocString       = oleaut32.NewProc("SysAllocString")
	procSysFreeString        = oleaut32.NewProc("SysFreeString")
	procVariantClear         = oleaut32.NewProc("VariantClear")

	clsidWbemLocator = windows.GUID{Data1: 0x4590f811, Data2: 0x1d3a, Data3: 0x11d0, Data4: [8]byte{0x89, 0x1f, 0x00, 0xaa, 0x00, 0x4b, 0x2e, 0x24}}
	iidIWbemLocator  = windows.GUID{Data1: 0xdc12a687, Data2: 0x737f, Data3: 0x11cf, Data4: [8]byte{0x88, 0x4d, 0x00, 0xaa, 0x00, 0x4b, 0x2e, 0x24}}
)

const (
	coinitMultithreaded       = 0x0
	clsctxInprocServer        = 0x1
	rpcCAuthnLevelDefault     = 0
	rpcCAuthnLevelCall        = 3
	rpcCImpLevelImpersonate   = 3
	rpcCAuthnWinNT            = 10
	rpcCAuthzNone             = 0
	eoacNone                  = 0
	wbemFlagForwardOnly       = 0x20
	wbemFlagReturnImmediately = 0x10
	wbemInfinite              = 0xFFFFFFFF
	rpcETooLate               = 0x80010119
	sFalse                    = 1
	rpcEChangedMode           = 0x80010106
	// wbemEInvalidNamespace: the namespace isn't there (no Hyper-V).
	wbemEInvalidNamespace = 0x8004100E
	// wbemEAccessDenied and E_ACCESSDENIED: not allowed to read it.
	wbemEAccessDenied = 0x80041003
	eAccessDenied     = 0x80070005

	vtNull = 1
	vtI2   = 2
	vtI4   = 3
	vtBSTR = 8
	vtBool = 11
	vtUI1  = 17
	vtUI2  = 18
	vtUI4  = 19
	vtI8   = 20
	vtUI8  = 21
)

// hresultError is a failed COM call.
type hresultError struct {
	call string
	hr   uint32
}

func (e *hresultError) Error() string { return fmt.Sprintf("%s: HRESULT 0x%08X", e.call, e.hr) }

func failed(hr uintptr) bool { return int32(hr) < 0 }

// coInit starts COM on this thread; coUninit ends it. coInit's error
// covers the thread already being in another mode, which is fine.
func coInit() (func(), error) {
	hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
	if failed(hr) && uint32(hr) != rpcEChangedMode {
		return nil, &hresultError{"CoInitializeEx", uint32(hr)}
	}
	// Once a process; later calls say it's too late, and that's fine.
	hr, _, _ = procCoInitializeSecurity.Call(0, ^uintptr(0), 0, 0, rpcCAuthnLevelDefault, rpcCImpLevelImpersonate, 0, eoacNone, 0)
	if failed(hr) && uint32(hr) != rpcETooLate {
		procCoUninitialize.Call()
		return nil, &hresultError{"CoInitializeSecurity", uint32(hr)}
	}
	return func() { procCoUninitialize.Call() }, nil
}

// iunknown is a COM object: its first word points at its vtable. Its
// memory is COM's, never Go's.
type iunknown struct{ vtbl *[32]uintptr }

// comObject is an interface pointer.
type comObject = *iunknown

func call(o comObject, i int, args ...uintptr) uintptr {
	hr, _, _ := syscall.SyscallN(o.vtbl[i], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return hr
}

func release(o comObject) {
	if o != nil {
		call(o, 2)
	}
}

func bstr(s string) uintptr {
	p, _ := windows.UTF16PtrFromString(s)
	b, _, _ := procSysAllocString.Call(uintptr(unsafe.Pointer(p)))
	return b
}

func freeBSTR(b uintptr) { procSysFreeString.Call(b) }

// connect opens a WMI namespace on this machine.
func connect(namespace string) (comObject, error) {
	var loc comObject
	hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidWbemLocator)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidIWbemLocator)), uintptr(unsafe.Pointer(&loc)))
	if failed(hr) {
		return nil, &hresultError{"CoCreateInstance(WbemLocator)", uint32(hr)}
	}
	defer release(loc)
	ns := bstr(namespace)
	defer freeBSTR(ns)
	var svc comObject
	// IWbemLocator::ConnectServer
	if hr := call(loc, 3, ns, 0, 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&svc))); failed(hr) {
		return nil, &hresultError{"ConnectServer(" + namespace + ")", uint32(hr)}
	}
	hr, _, _ = procCoSetProxyBlanket.Call(uintptr(unsafe.Pointer(svc)), rpcCAuthnWinNT, rpcCAuthzNone, 0, rpcCAuthnLevelCall, rpcCImpLevelImpersonate, 0, eoacNone)
	if failed(hr) {
		release(svc)
		return nil, &hresultError{"CoSetProxyBlanket", uint32(hr)}
	}
	return svc, nil
}

// row is one result's properties, by name.
type row map[string]any

func (r row) str(k string) string {
	s, _ := r[k].(string)
	return s
}

func (r row) int(k string) int64 {
	switch v := r[k].(type) {
	case int64:
		return v
	case string: // WMI gives 64-bit integers as strings
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	case bool:
		if v {
			return 1
		}
	}
	return 0
}

// variant is OLE's VARIANT on 64-bit Windows (24 bytes).
type variant struct {
	vt  uint16
	_   [3]uint16
	val [2]uint64
}

// query runs WQL and reads the named properties of every result.
func query(svc comObject, wql string, props []string) ([]row, error) {
	lang, q := bstr("WQL"), bstr(wql)
	defer freeBSTR(lang)
	defer freeBSTR(q)
	var enum comObject
	// IWbemServices::ExecQuery
	if hr := call(svc, 20, lang, q, wbemFlagForwardOnly|wbemFlagReturnImmediately, 0, uintptr(unsafe.Pointer(&enum))); failed(hr) {
		return nil, &hresultError{"ExecQuery", uint32(hr)}
	}
	defer release(enum)
	names := make([]*uint16, len(props))
	for i, p := range props {
		names[i], _ = windows.UTF16PtrFromString(p)
	}
	var out []row
	for {
		var obj comObject
		var n uint32
		// IEnumWbemClassObject::Next
		hr := call(enum, 4, wbemInfinite, 1, uintptr(unsafe.Pointer(&obj)), uintptr(unsafe.Pointer(&n)))
		if failed(hr) {
			return out, &hresultError{"Next", uint32(hr)}
		}
		if n == 0 {
			break
		}
		r := row{}
		for i, p := range props {
			var v variant
			// IWbemClassObject::Get
			if hr := call(obj, 4, uintptr(unsafe.Pointer(names[i])), 0, uintptr(unsafe.Pointer(&v)), 0, 0); !failed(hr) {
				if x, ok := v.value(); ok {
					r[p] = x
				}
				procVariantClear.Call(uintptr(unsafe.Pointer(&v)))
			}
		}
		release(obj)
		out = append(out, r)
		if hr == sFalse {
			break
		}
	}
	return out, nil
}

func (v *variant) value() (any, bool) {
	switch v.vt {
	case vtBSTR:
		p := *(*unsafe.Pointer)(unsafe.Pointer(&v.val[0]))
		if p == nil {
			return "", true
		}
		n := *(*uint32)(unsafe.Add(p, -4)) / 2 // a BSTR's length in bytes sits just before it
		return windows.UTF16ToString(unsafe.Slice((*uint16)(p), n)), true
	case vtI2:
		return int64(int16(v.val[0])), true
	case vtI4:
		return int64(int32(v.val[0])), true
	case vtUI1:
		return int64(uint8(v.val[0])), true
	case vtUI2:
		return int64(uint16(v.val[0])), true
	case vtUI4:
		return int64(uint32(v.val[0])), true
	case vtI8, vtUI8:
		return int64(v.val[0]), true
	case vtBool:
		return int16(v.val[0]) != 0, true
	}
	return nil, false
}
