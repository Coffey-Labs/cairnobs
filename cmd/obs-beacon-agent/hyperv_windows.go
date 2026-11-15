package main

import (
	"errors"

	"github.com/cairnobs/cairnobs/internal/hyperv"
	"github.com/cairnobs/cairnobs/internal/report"
)

// readHyperV reads the host's Hyper-V VMs (docs/windows.md, step 8): nil
// where Hyper-V isn't installed, so the report has no vms field.
func (a *agent) readHyperV() *report.VMs {
	vms, err := hyperv.Read()
	switch {
	case errors.Is(err, hyperv.ErrAbsent):
		return nil
	case err != nil:
		a.once("hyperv:"+err.Error(), "Hyper-V: %v", err)
		return &report.VMs{Error: err.Error()}
	}
	recs := hyperv.Records(vms)
	a.stampSince(recs)
	return &report.VMs{List: recs}
}
