/*
Copyright 2018 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

import (
	"fmt"
	"strings"

	"github.com/gravitational/kingpin"
	"github.com/gravitational/teleport/lib/asciitable"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/service"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/trace"
)

// StatusCommand implements `tctl token` group of commands
type StatusCommand struct {
	config *service.Config

	// CLI clauses (subcommands)
	status *kingpin.CmdClause
}

// Initialize allows StatusCommand to plug itself into the CLI parser
func (c *StatusCommand) Initialize(app *kingpin.Application, config *service.Config) {
	c.config = config
	c.status = app.Command("status", "Report cluster status")
}

// TryRun takes the CLI command as an argument (like "nodes ls") and executes it.
func (c *StatusCommand) TryRun(cmd string, client auth.ClientI) (match bool, err error) {
	switch cmd {
	case c.status.FullCommand():
		err = c.Status(client)
	default:
		return false, nil
	}
	return true, trace.Wrap(err)
}

// onStatus is called to execute "status"
func (c *StatusCommand) Status(client auth.ClientI) error {
	clusterNameResource, err := client.GetClusterName()
	if err != nil {
		return trace.Wrap(err)
	}
	clusterName := clusterNameResource.GetClusterName()

	hostCAs, err := client.GetCertAuthorities(services.HostCA, false)
	if err != nil {
		return trace.Wrap(err)
	}

	userCAs, err := client.GetCertAuthorities(services.UserCA, false)
	if err != nil {
		return trace.Wrap(err)
	}

	authorities := append(userCAs, hostCAs...)
	view := func() string {
		table := asciitable.MakeTable([]string{"Parameter", "Status and description"})
		table.AddRow([]string{"Local Cluster Name", clusterName})
		for _, ca := range authorities {
			local := "Local"
			if ca.GetClusterName() != clusterName {
				local = "Remote"
			}
			info := fmt.Sprintf("%v %v CA", local, strings.Title(string(ca.GetType())))
			rotation := ca.GetRotation()
			table.AddRow([]string{info, rotation.String()})
		}
		return table.AsBuffer().String()
	}
	fmt.Printf(view())
	return nil
}
