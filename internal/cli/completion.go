package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// version prints what this binary is. It drives no route, so it works on a
// machine that has never logged in and has no server configured.
func (r *runner) version(inv *invocation) error {
	if r.out.JSON() {
		data, err := json.Marshal(map[string]any{
			"version": r.env.Version,
			"commit":  r.env.Commit,
		})
		if err != nil {
			return &ContractError{Msg: "the version could not be encoded"}
		}
		return r.out.Emit(&Response{Status: http.StatusOK, Data: data, Warnings: []string{}})
	}
	// Version first, because that is what an owner reads back to a release
	// page; the commit follows it, because that is what a bug report needs.
	_, _ = fmt.Fprintf(r.out.stdout, "agm %s (%s)\n", r.env.Version, r.env.Commit)
	return nil
}

// completion prints a shell completion script. It drives no route either.
func (r *runner) completion(inv *invocation) error {
	shell := inv.positional(0)
	switch shell {
	case "bash":
		_, _ = fmt.Fprint(r.out.stdout, bashCompletion())
	case "zsh":
		_, _ = fmt.Fprint(r.out.stdout, zshCompletion())
	case "fish":
		_, _ = fmt.Fprint(r.out.stdout, fishCompletion())
	default:
		return usageErr("`agm completion` takes bash, zsh or fish, not %q", shell)
	}
	return nil
}

// completionWords is every command word sequence and every flag, so a
// completion script cannot fall behind the command table.
func completionWords() (commands []string, flags []string) {
	seenFlag := map[string]bool{}
	for _, c := range commandTable() {
		commands = append(commands, c.Name())
		for _, f := range c.allFlags() {
			if !seenFlag[f.name] {
				seenFlag[f.name] = true
				flags = append(flags, f.name)
			}
		}
	}
	sort.Strings(commands)
	sort.Strings(flags)
	return commands, flags
}

func bashCompletion() string {
	commands, flags := completionWords()
	var b strings.Builder
	b.WriteString("# bash completion for agm\n")
	b.WriteString("_agm() {\n")
	_, _ = fmt.Fprintf(&b, "  local commands=%q\n", strings.Join(commands, " "))
	_, _ = fmt.Fprintf(&b, "  local flags=%q\n", strings.Join(flags, " "))
	b.WriteString(`  local cur="${COMP_WORDS[COMP_CWORD]}"
  if [[ "$cur" == -* ]]; then
    COMPREPLY=( $(compgen -W "$flags" -- "$cur") )
  else
    COMPREPLY=( $(compgen -W "$commands" -- "$cur") )
  fi
}
complete -F _agm agm
`)
	return b.String()
}

func zshCompletion() string {
	commands, flags := completionWords()
	var b strings.Builder
	b.WriteString("#compdef agm\n")
	b.WriteString("_agm() {\n")
	_, _ = fmt.Fprintf(&b, "  local -a commands flags\n  commands=(%s)\n", quoteAll(commands))
	_, _ = fmt.Fprintf(&b, "  flags=(%s)\n", quoteAll(flags))
	b.WriteString(`  if [[ "$words[$CURRENT]" == -* ]]; then
    compadd -a flags
  else
    compadd -a commands
  fi
}
compdef _agm agm
`)
	return b.String()
}

func fishCompletion() string {
	commands, flags := completionWords()
	var b strings.Builder
	b.WriteString("# fish completion for agm\n")
	for _, c := range commands {
		_, _ = fmt.Fprintf(&b, "complete -c agm -a %q\n", c)
	}
	for _, f := range flags {
		_, _ = fmt.Fprintf(&b, "complete -c agm -l %q\n", strings.TrimPrefix(f, "--"))
	}
	return b.String()
}

func quoteAll(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = fmt.Sprintf("%q", w)
	}
	return strings.Join(quoted, " ")
}
