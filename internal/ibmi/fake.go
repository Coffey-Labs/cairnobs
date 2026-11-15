package ibmi

import (
	"strings"

	"github.com/cairnobs/cairnobs/internal/mapepire"
)

// FakePartition is a partition as IBM documents the views, for tests and
// the demo simulator: what Answer gives a mapepire.Fake. Change its fields
// between reads to make trouble.
type FakePartition struct {
	HostName, PartitionName string
	Elapsed, CPUUsed        float64 // ELAPSED_TIME and ELAPSED_CPU_USED
	SystemASPUsed           float64
	Attention, Restricted   bool
	ASPs                    []mapepire.Row
	NoTR                    bool // no *USE on WRKPTFGRP
}

// NewFakePartition is a healthy 7.5 TR6 partition with a system ASP and
// one independent ASP.
func NewFakePartition(host string) *FakePartition {
	return &FakePartition{HostName: strings.ToUpper(host), PartitionName: strings.ToUpper(host), SystemASPUsed: 61.4,
		ASPs: []mapepire.Row{
			{"ASP_NUMBER": 1.0, "DEVICE_DESCRIPTION_NAME": nil, "RESOURCE_NAME": nil, "ASP_STATE": "NONE", "ASP_TYPE": "SYSTEM",
				"NUMBER_OF_DISK_UNITS": 8.0, "DISK_UNITS_PRESENT": "ALL", "TOTAL_CAPACITY": 2_400_000.0, "TOTAL_CAPACITY_AVAILABLE": 926_400.0,
				"STORAGE_THRESHOLD_PERCENTAGE": 90.0, "OVERFLOW_STORAGE": 0.0},
			{"ASP_NUMBER": 33.0, "DEVICE_DESCRIPTION_NAME": "PRODIASP", "RESOURCE_NAME": "IASP1", "ASP_STATE": "AVAILABLE", "ASP_TYPE": "PRIMARY",
				"NUMBER_OF_DISK_UNITS": 4.0, "DISK_UNITS_PRESENT": "ALL", "TOTAL_CAPACITY": 1_200_000.0, "TOTAL_CAPACITY_AVAILABLE": 540_000.0,
				"STORAGE_THRESHOLD_PERCENTAGE": 90.0, "OVERFLOW_STORAGE": nil},
		}}
}

// Answer gives a query's rows by the view it reads.
func (p *FakePartition) Answer(sql string) ([]mapepire.Row, string) {
	yes := func(b bool) string {
		if b {
			return "YES"
		}
		return "NO"
	}
	on := "OFF"
	if p.Attention {
		on = "ON"
	}
	switch {
	case strings.Contains(sql, "QSYS2.SYSTEM_STATUS_INFO_BASIC"):
		return []mapepire.Row{{"HOST_NAME": p.HostName, "PARTITION_ID": 3.0, "PARTITION_NAME": p.PartitionName, "NUMBER_OF_PARTITIONS": 6.0,
			"ELAPSED_TIME": p.Elapsed, "ELAPSED_CPU_USED": p.CPUUsed, "CONFIGURED_CPUS": 4.0, "CURRENT_CPU_CAPACITY": 2.5,
			"MAIN_STORAGE_SIZE": 268_435_456.0, "SYSTEM_ASP_STORAGE": 2_400_000.0, "SYSTEM_ASP_USED": p.SystemASPUsed,
			"CURRENT_TEMPORARY_STORAGE": 41_250.0, "TOTAL_JOBS_IN_SYSTEM": 18_204.0, "MAXIMUM_JOBS_IN_SYSTEM": 163_520.0,
			"ACTIVE_JOBS_IN_SYSTEM": 612.0, "ACTIVE_THREADS_IN_SYSTEM": 2_870.0, "RESTRICTED_STATE": yes(p.Restricted),
			"MACHINE_TYPE": "9105", "MACHINE_MODEL": "42A ", "SERIAL_NUMBER": "78ABC12 ", "ATTENTION_LIGHT": on}}, ""
	case strings.Contains(sql, "SYSIBMADM.ENV_SYS_INFO"):
		return []mapepire.Row{{"OS_VERSION": "7", "OS_RELEASE": "5"}}, ""
	case strings.Contains(sql, "QSYS2.GROUP_PTF_INFO"):
		if p.NoTR {
			return nil, "[SQL0551] Not authorized to object WRKPTFGRP in QSYS type *CMD."
		}
		return []mapepire.Row{
			{"PTF_GROUP_NAME": "SF99957", "PTF_GROUP_DESCRIPTION": "TECHNOLOGY REFRESH", "LEVEL": 6.0},
			{"PTF_GROUP_NAME": "SF99956", "PTF_GROUP_DESCRIPTION": "TECHNOLOGY REFRESH PLUS RECOMMENDED", "LEVEL": 7.0},
		}, ""
	case strings.Contains(sql, "QSYS2.ASP_INFO"):
		return p.ASPs, ""
	}
	return nil, "[SQL0204] not found."
}
