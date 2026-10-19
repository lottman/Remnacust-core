package api

import (
	"fmt"
	"os"

	"github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/main/commands/base"
)

// API stdout is a machine-readable response. Configuration diagnostics must
// not become part of that response, including warnings emitted during Build.
type apiDiagnosticHandler struct{}

func (apiDiagnosticHandler) Handle(message log.Message) {
	fmt.Fprintln(os.Stderr, message.String())
}

func init() {
	for _, command := range CmdAPI.Commands {
		if command.Run == nil {
			continue
		}
		run := command.Run
		command.Run = func(cmd *base.Command, args []string) {
			// Write synchronously so short-lived commands retain their warnings.
			log.RegisterHandler(apiDiagnosticHandler{})
			run(cmd, args)
		}
	}
}

// CmdAPI calls an API in an Xray process
var CmdAPI = &base.Command{
	UsageLine: "{{.Exec}} api",
	Short:     "Call an API in an Xray process",
	Long: `{{.Exec}} {{.LongName}} provides tools to manipulate Xray via its API.
`,
	Commands: append(runtimeControlCommands(), []*base.Command{
		cmdRestartLogger,
		cmdGetStats,
		cmdQueryStats,
		cmdUsageReceipt,
		cmdSysStats,
		cmdBalancerInfo,
		cmdBalancerOverride,
		cmdAddInbounds,
		cmdAddOutbounds,
		cmdRemoveInbounds,
		cmdRemoveOutbounds,
		cmdListInbounds,
		cmdListOutbounds,
		cmdAddInboundUsers,
		cmdRemoveInboundUsers,
		cmdInboundUser,
		cmdInboundUserCount,
		cmdAddRules,
		cmdRemoveRules,
		cmdListRules,
		cmdSourceIpBlock,
		cmdOnlineStats,
		cmdOnlineStatsIpList,
		cmdGetAllOnlineUsers,
	}...),
}
