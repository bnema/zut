package agent

import "io"

// botSpec describes one bot protocol to the generic daemon CLI in
// botcmd.go. Everything protocol-specific (setup flow, status text,
// state files) hangs off this struct; the subcommand plumbing
// (start/stop/logs through the daemon) is shared. name is also the daemon
// bot kind.
type botSpec struct {
	name       string   // "telegram", "matrix" — used in messages
	subcommand string   // "telegram-bot", "matrix-bot"
	aliases    []string // {"tg"}, {"mx"}

	// configured reports whether setup has been completed (e.g. a
	// token is present), with a hint error message when it hasn't.
	configured func(zutHome string) (bool, error)

	printHelp func()
	setup     func(tail []string) error
	status    func(w io.Writer, version string) error
	reset     func() error
}

// botSpecs is the registry the dispatcher walks. Matrix is appended
// in matrixcmd.go's init.
var botSpecs = []*botSpec{telegramSpec()}

// specFor returns the spec matching subcommand or one of its aliases.
func specFor(subcommand string) *botSpec {
	for _, s := range botSpecs {
		if s.subcommand == subcommand {
			return s
		}
		for _, a := range s.aliases {
			if a == subcommand {
				return s
			}
		}
	}
	return nil
}
