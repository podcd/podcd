// Package deploy carries the files a host needs to run the agent
package deploy

import _ "embed"

// AgentService is the systemd user unit that runs `podcd run`.
// * `podcd install` writes this file;
// * deploy/bootstrap.sh calls `podcd install`.
//
//go:embed podcd-agent.service
var AgentService []byte
