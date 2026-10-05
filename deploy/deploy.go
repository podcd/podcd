// Package deploy carries the files a host needs to run the agent
package deploy

import _ "embed"

// AgentService is the systemd user unit `podcd install` writes.
//
//go:embed podcd-agent.service
var AgentService []byte
