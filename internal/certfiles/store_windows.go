//go:build windows

package certfiles

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/cairnobs/cairnobs/internal/certinfo"
	"github.com/cairnobs/cairnobs/internal/report"
)

// StoreDefaults are the machine's certificate stores a Windows agent reads
// when agent.yaml doesn't say (cert_stores): Personal, where IIS, RDP
// gateways, SQL Server and most services keep theirs, and Web Hosting,
// IIS's own.
var StoreDefaults = []string{"My", "WebHosting"}

// ReadStores is the certificates in the machine's stores (LocalMachine\
// name), opened read-only: only the certificates, never a key, which stays
// where Windows keeps it. A CA's certificate without names is left out, as
// in files. Each is at Cert:\LocalMachine\<store>\<thumbprint>, the path
// PowerShell takes. A store the machine doesn't have is skipped.
func ReadStores(names []string) ([]report.CertFile, error) {
	var out []report.CertFile
	var errs []error
	for _, name := range names {
		list, err := readStore(name)
		if err != nil {
			errs = append(errs, fmt.Errorf("LocalMachine\\%s: %w", name, err))
			continue
		}
		out = append(out, list...)
	}
	return out, errors.Join(errs...)
}

func readStore(name string) ([]report.CertFile, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM, 0, 0,
		windows.CERT_SYSTEM_STORE_LOCAL_MACHINE|windows.CERT_STORE_READONLY_FLAG|windows.CERT_STORE_OPEN_EXISTING_FLAG,
		uintptr(unsafe.Pointer(n)))
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, nil
		}
		return nil, err
	}
	defer windows.CertCloseStore(h, 0)
	var out []report.CertFile
	var c *windows.CertContext
	for {
		c, err = windows.CertEnumCertificatesInStore(h, c)
		if c == nil {
			break
		}
		der := bytes.Clone(unsafe.Slice(c.EncodedCert, c.Length))
		x, err := x509.ParseCertificate(der)
		if err != nil || (x.IsCA && len(certinfo.Names(x)) == 0) {
			continue
		}
		out = append(out, report.CertFile{Path: StorePath(name, der), Mod: x.NotBefore, Cert: *certinfo.Of(x)})
	}
	return out, nil
}
