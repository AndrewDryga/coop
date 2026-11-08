package fsfilter

import "strings"

// AppArmorProfile names the profile `coop setup apparmor` loads for the filesystem helper under
// Docker's systemd cgroup driver. Docker's default profile refuses every system bus message, so
// without this profile the helper runs unconfined. The version suffix changes whenever the policy
// does, so a host knows whether the loaded policy is the one this Coop expects.
const AppArmorProfile = "coop-filesystem-helper-v1"

// AppArmorProfilePrefix matches every version of the profile, so an update can retire old ones.
const AppArmorProfilePrefix = "coop-filesystem-helper-v"

// AppArmorPolicy is Docker's default container profile (moby's profiles/apparmor template) with
// the helper's own name for its peers, plus the only system bus calls the helper makes: Hello, then
// systemd's StartTransientUnit and StopUnit for Coop's slices. Mounts and the /proc and /sys writes
// Docker denies stay denied, and so does every other bus call. AppArmor cannot check a call's
// arguments, so these two still give systemd's full power: the profile does not limit what the
// helper could do through systemd.
func AppArmorPolicy() string {
	return strings.ReplaceAll(appArmorTemplate, "@NAME@", AppArmorProfile)
}

const appArmorTemplate = `# Written by coop setup apparmor for Coop's filesystem protection helper. Coop rewrites it.
#include <tunables/global>

profile @NAME@ flags=(attach_disconnected,mediate_deleted) {
  #include <abstractions/base>

  network,
  capability,
  file,
  umount,

  signal (receive) peer=unconfined,
  signal (receive) peer=runc,
  signal (receive) peer=crun,
  signal (receive) peer=dockerd,
  signal (send,receive) peer=@NAME@,

  dbus send bus=system path=/org/freedesktop/DBus interface=org.freedesktop.DBus member=Hello peer=(name=org.freedesktop.DBus),
  dbus send bus=system path=/org/freedesktop/systemd1 interface=org.freedesktop.systemd1.Manager member=StartTransientUnit peer=(name=org.freedesktop.systemd1),
  dbus send bus=system path=/org/freedesktop/systemd1 interface=org.freedesktop.systemd1.Manager member=StopUnit peer=(name=org.freedesktop.systemd1),

  deny @{PROC}/* w,
  deny @{PROC}/{[^1-9],[^1-9][^0-9],[^1-9s][^0-9y][^0-9s],[^1-9][^0-9][^0-9][^0-9/]*}/** w,
  deny @{PROC}/sys/[^k]** w,
  deny @{PROC}/sys/kernel/{?,??,[^s][^h][^m]**} w,
  deny @{PROC}/sysrq-trigger rwklx,
  deny @{PROC}/kcore rwklx,

  deny mount,

  deny /sys/[^f]*/** wklx,
  deny /sys/f[^s]*/** wklx,
  deny /sys/fs/[^c]*/** wklx,
  deny /sys/fs/c[^g]*/** wklx,
  deny /sys/fs/cg[^r]*/** wklx,
  deny /sys/firmware/** rwklx,
  deny /sys/devices/virtual/powercap/** rwklx,
  deny /sys/kernel/security/** rwklx,

  ptrace (trace,read,tracedby,readby) peer=@NAME@,
}
`
