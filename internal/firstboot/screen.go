package firstboot

import (
	"strings"
	"unicode/utf8"
)

const (
	clearSequence = "\x1b[H\x1b[2J"
	brandBlue     = "\x1b[38;5;32m"
	resetColour   = "\x1b[0m"
)

// logo spells OmniTrust in at most 60 columns.
var logo = []string{
	" ###  #   # #   #  ###  ##### ####  #   #  #### #####",
	"#   # ## ## ##  #   #     #   #   # #   # #       #  ",
	"#   # # # # # # #   #     #   ####  #   #  ###    #  ",
	"#   # #   # #  ##   #     #   #  #  #   #     #   #  ",
	" ###  #   # #   #  ###    #   #   #  ###  ####    #  ",
}

// renderScreen returns the local console screen: the logo, then the setup lines if there are any, else the
// Appliance address and the state. Lines longer than width are wrapped. It carries only public information.
func renderScreen(width int, version, hostName, nodeIP string, state State, lines []string) string {
	var b strings.Builder
	b.WriteString(clearSequence)
	b.WriteString(brandBlue)
	b.WriteString(strings.Join(logo, "\n"))
	b.WriteString(resetColour)
	b.WriteString("\n\n")
	if lines == nil {
		if nodeIP == "" {
			nodeIP = "unknown"
		}
		lines = []string{
			"ILM Appliance " + version,
			"https://" + hostName + " (node " + nodeIP + ")",
			"State: " + string(state),
		}
	}
	for _, line := range lines {
		for _, part := range wrap(line, width) {
			b.WriteString(part)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// wrap splits line into parts of at most width columns, breaking after a space or a colon where it can.
func wrap(line string, width int) []string {
	var parts []string
	for width > 0 && utf8.RuneCountInString(line) > width {
		runes := []rune(line)
		cut := width
		for i := width; i > 0; i-- {
			if runes[i] == ' ' || runes[i-1] == ':' {
				cut = i
				break
			}
		}
		parts = append(parts, strings.TrimRight(string(runes[:cut]), " "))
		line = strings.TrimLeft(string(runes[cut:]), " ")
	}
	return append(parts, line)
}
