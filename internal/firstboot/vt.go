package firstboot

// vtSwitcher brings virtual terminal n to the front.
type vtSwitcher func(n int) error

// consoleVT is the virtual terminal the OmniTrust screen is drawn on.
const consoleVT = 1
