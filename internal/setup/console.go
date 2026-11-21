package setup

import (
	"io"
	"log/slog"
	"net/netip"
	"strings"

	"github.com/OmniTrustILM/appliance-ng/internal/auth"
)

// Port is the port wizard part 1 is served on.
const Port = "8443"

// LoadCode returns the setup code preset by the hypervisor in the SMBIOS OEM strings of the table at dmiPath, or a
// new random code drawn from rand. display is the code to show on the console, empty for a preset code. Neither the
// code nor the preset hash is ever logged.
func LoadCode(dmiPath string, rand io.Reader, log *slog.Logger) (code *Code, display string) {
	strs, err := ReadOEMStrings(dmiPath)
	if err != nil {
		log.Debug("no SMBIOS OEM strings, generating a setup code", "error", err)
	}
	for _, s := range strs {
		hash, ok := strings.CutPrefix(s, PresetKey)
		if !ok {
			continue
		}
		if code, err := PresetCode(hash); err == nil {
			log.Info("setup code preset by the hypervisor")
			return code, ""
		}
		log.Warn("the preset setup code is malformed, generating a setup code")
		break
	}
	return NewCode(rand)
}

// ConsoleLines returns the lines the local console shows while wizard part 1 runs at ip: the version, the setup
// address, the certificate fingerprint and the setup code display, or that the code is preset when it is empty.
func ConsoleLines(version string, ip netip.Addr, fingerprint, display string) []string {
	code := "Setup code: preset by the hypervisor"
	if display != "" {
		code = "Setup code " + display
	}
	return []string{
		"ILM appliance " + version + " setup",
		"Open https://" + ip.String() + ":" + Port + auth.BasePath + "/setup/",
		"Certificate SHA-256 " + fingerprint,
		code,
	}
}
