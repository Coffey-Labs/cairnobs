package main

// Windows keeps the config and state under ProgramData; `service install`
// gives the folder to SYSTEM, Administrators and the service's own account
// only (docs/windows.md).
const (
	dataDir         = `C:\ProgramData\obs-beacon`
	defaultConfig   = dataDir + `\agent.yaml`
	defaultStateDir = dataDir + `\state`
	examplesDir     = `agent.yaml.example in the zip`
	hasHelper       = false // the agent reads what it needs itself
)
