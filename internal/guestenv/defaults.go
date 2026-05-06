package guestenv

const (
	// DefaultHome is the guest home used by policy normalization and guest command
	// environment fallback. Changing it can change block command behavior, so
	// dependency/service volume producer versions must change with it.
	DefaultHome = "/root"

	// DefaultPath is the guest PATH used when neither ambient nor request env
	// supplies one. Changing it has the same producer-version requirement as
	// DefaultHome.
	DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:" + DefaultHome + "/.local/bin"
)
