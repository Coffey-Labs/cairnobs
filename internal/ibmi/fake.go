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
	Disks                   []mapepire.Row
	Subsystems              []mapepire.Row
	Interfaces              []mapepire.Row
	Messages                []mapepire.Row // QSYSOPR's, newest first: see Msg
	JobQueues               []mapepire.Row // with released jobs: see JobQ
	NoTR                    bool           // no *USE on WRKPTFGRP
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
		},
		Disks: []mapepire.Row{
			{"ASP_NUMBER": 1.0, "UNIT_NUMBER": 1.0, "RESOURCE_NAME": "DMP001", "HARDWARE_STATUS": "ACTIVE", "PROTECTION_STATUS": "ACTIVE", "SSD_PFA_WARNING": "NO"},
			{"ASP_NUMBER": 1.0, "UNIT_NUMBER": 2.0, "RESOURCE_NAME": "DMP002", "HARDWARE_STATUS": "ACTIVE", "PROTECTION_STATUS": "ACTIVE", "SSD_PFA_WARNING": nil},
			{"ASP_NUMBER": 33.0, "UNIT_NUMBER": 4001.0, "RESOURCE_NAME": "DMP041", "HARDWARE_STATUS": "BUSY", "PROTECTION_STATUS": nil, "SSD_PFA_WARNING": nil},
		},
		Subsystems: []mapepire.Row{
			sbs("QSYS", "QCTL", "ACTIVE", 3, true), sbs("QSYS", "QINTER", "ACTIVE", 41, false), sbs("QSYS", "QBATCH", "ACTIVE", 12, false),
			sbs("QSYS", "QUSRWRK", "ACTIVE", 88, false), sbs("QSYS", "QSPL", "INACTIVE", 0, false), sbs("ORDLIB", "ORDERS", "ACTIVE", 7, false),
		},
		Interfaces: []mapepire.Row{
			iface("127.0.0.1", "*LOOPBACK", "ACTIVE", true), iface("10.0.4.20", "ETHLINE", "ACTIVE", true),
			iface("10.0.9.20", "ETHBKP", "INACTIVE", false), iface("fe80::1", "ETHLINE", "ACTIVE", true),
		},
		// An inquiry answered an hour after it came, and the reply.
		Messages: []mapepire.Row{
			Msg("0000A2C0", "000098F0", "", "REPLY", "G", 0, "654321/QSYSOPR/QPADEV0001", 3*3600),
			Msg("000098F0", "", "CPA4002", "INQUIRY", "Verify alignment on device PRT01. (I C G N R)", 0, "654320/ACCT/PRTJOB", 4*3600),
		},
		JobQueues: []mapepire.Row{JobQ("QSYS", "QBATCH", false, 2, "QBATCH")}}
}

// Msg is one of QSYSOPR's rows as the message read selects it.
func Msg(key, assoc, id, typ, text string, sev float64, job string, ageSeconds float64) mapepire.Row {
	r := mapepire.Row{"MSG_KEY": key, "ASSOC_KEY": nil, "MESSAGE_ID": nil, "MESSAGE_TYPE": typ, "MESSAGE_TEXT": text, "SEVERITY": sev,
		"FROM_JOB": job, "FROM_USER": strings.Split(job, "/")[1], "AGE_SECONDS": ageSeconds}
	if assoc != "" {
		r["ASSOC_KEY"] = assoc
	}
	if id != "" {
		r["MESSAGE_ID"] = id
	}
	return r
}

// JobQ is one job queue with jobs ready to run.
func JobQ(lib, name string, held bool, waiting float64, subsystem string) mapepire.Row {
	r := mapepire.Row{"JOB_QUEUE_LIBRARY": lib, "JOB_QUEUE_NAME": name, "JOB_QUEUE_STATUS": "RELEASED", "RELEASED_JOBS": waiting, "SUBSYSTEM_NAME": nil}
	if held {
		r["JOB_QUEUE_STATUS"] = "HELD"
	}
	if subsystem != "" {
		r["SUBSYSTEM_NAME"] = subsystem
	}
	return r
}

func sbs(lib, name, status string, jobs float64, ctl bool) mapepire.Row {
	c := "NO"
	if ctl {
		c = "YES"
	}
	return mapepire.Row{"SUBSYSTEM_DESCRIPTION_LIBRARY": lib, "SUBSYSTEM_DESCRIPTION": name, "STATUS": status, "CURRENT_ACTIVE_JOBS": jobs,
		"CONTROLLING_SUBSYSTEM": c, "TEXT_DESCRIPTION": nil}
}

func iface(addr, line, status string, auto bool) mapepire.Row {
	a := "NO"
	if auto {
		a = "YES"
	}
	return mapepire.Row{"CONNECTION_TYPE": "IPV4", "INTERNET_ADDRESS": addr, "PREFIX_LENGTH": 24.0, "LINE_DESCRIPTION": line,
		"INTERFACE_STATUS": status, "AUTOSTART": a, "INTERFACE_TEXT": nil}
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
	case strings.Contains(sql, "QSYS2.SYSDISKSTAT"):
		return p.Disks, ""
	case strings.Contains(sql, "QSYS2.SUBSYSTEM_INFO"):
		return p.Subsystems, ""
	case strings.Contains(sql, "QSYS2.NETSTAT_INTERFACE_INFO"):
		return p.Interfaces, ""
	case strings.Contains(sql, "QSYS2.MESSAGE_QUEUE_INFO"):
		return p.Messages, ""
	case strings.Contains(sql, "QSYS2.JOB_QUEUE_INFO"):
		return p.JobQueues, ""
	}
	return nil, "[SQL0204] not found."
}
