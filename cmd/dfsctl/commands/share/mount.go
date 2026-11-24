package share

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/spf13/cobra"
)

// defaultPort is protocol's adapter's port when its record names none: the
// adapter settings' default, which dfsctl may not link, so a test holds the
// two equal.
func defaultPort(protocol string) (int, bool) {
	switch protocol {
	case "nfs":
		return 12049, true
	case "smb":
		return 12445, true
	}
	return 0, false
}

func newMountCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mount [share] <mountpoint>",
		Short: "Mount a share on this host over NFS or SMB",
		Long: `Mount a share at a mount point on this host, over NFS or SMB, from the server
the current context names, on the port its adapter's record gives.

The command runs the host's own mount command, which typically needs root.
For SMB that command asks for the password itself; dfsctl never handles it.
--dry-run prints the command instead of running it.

Examples:
  # Mount over NFS
  sudo dfsctl share mount archive /mnt/archive --protocol nfs

  # Mount over NFSv4.1
  sudo dfsctl share mount archive /mnt/archive --protocol nfs --nfs-version 4.1

  # Mount over SMB as alice
  sudo dfsctl share mount archive /mnt/archive --protocol smb --username alice

  # Map a drive on Windows
  dfsctl share mount archive Z: --protocol smb`,
		Args: func(cmd *cobra.Command, args []string) error {
			want := 2
			if id, _ := cmd.Flags().GetString("id"); id != "" {
				want = 1
			}
			if len(args) != want {
				return fmt.Errorf("%s: name the share by its name or by --id, exactly one, and the mount point", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, target, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			path := cmd.CommandPath()
			id, err := cmdutil.Resolve(cmd, c.Shares(), "share", args[:len(args)-1])
			if err != nil {
				return err
			}
			sh, err := c.Shares().Get(cmd.Context(), id)
			if err != nil {
				return fmt.Errorf("%s: share %s: %w", path, id, err)
			}
			m := mount{GOOS: runtime.GOOS, Host: "localhost", Share: sh.Spec.Path, Point: args[len(args)-1]}
			m.Protocol, _ = cmd.Flags().GetString("protocol")
			m.NFSVersion, _ = cmd.Flags().GetString("nfs-version")
			m.User, _ = cmd.Flags().GetString("username")
			if m.User == "" {
				m.User = target.Dialled.Username
			}
			if os.Geteuid() == 0 {
				m.SudoUser, m.UID, m.GID = os.Getenv("SUDO_USER"), os.Getenv("SUDO_UID"), os.Getenv("SUDO_GID")
			}
			if m.Protocol == "smb" {
				m.Share = sh.Spec.Name
			}
			// A context naming a local socket has no host: the server is this one.
			if u, err := url.Parse(target.Dialled.ServerURL); err == nil && u.Hostname() != "" {
				m.Host = u.Hostname()
			}
			if m.Port, err = adapterPort(cmd, c, m.Protocol); err != nil {
				return err
			}
			line, err := mountLine(m)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if dry, _ := cmd.Flags().GetBool("dry-run"); dry {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), strings.Join(line, " "))
				return err
			}
			// decision: share mount is the one command that does more than call
			// the API: it runs the host's mount command. It reads only the share
			// and its adapter through the API, and no store or bootstrap, so the
			// CLI still links nothing of the server.
			x := exec.CommandContext(cmd.Context(), line[0], line[1:]...)
			x.Stdin, x.Stdout, x.Stderr = env.In, cmd.OutOrStdout(), cmd.ErrOrStderr()
			if err := x.Run(); err != nil {
				return fmt.Errorf("%s: %s: %w", path, strings.Join(line, " "), err)
			}
			return nil
		},
	}
	cmd.Flags().String("id", "", "The share's ID, in place of its name")
	cmd.Flags().StringP("protocol", "p", "", "The protocol to mount over: nfs or smb")
	cmd.Flags().String("nfs-version", "3", "The NFS version: 3, 4, 4.0, 4.1 or 4.2")
	cmd.Flags().StringP("username", "u", "", "The SMB user; the current context's account when not given")
	cmd.Flags().Bool("dry-run", false, "Print the mount command instead of running it")
	_ = cmd.MarkFlagRequired("protocol")
	return cmd
}

// adapterPort is the port protocol's adapter record names, or the adapter's
// default when it names none.
//
// decision: the port is the installation's record, not a node override:
// dfsctl does not know which node the host it mounts from is.
func adapterPort(cmd *cobra.Command, c *client.Client, protocol string) (int, error) {
	port, ok := defaultPort(protocol)
	if !ok {
		return 0, fmt.Errorf("%s: --protocol %q: want nfs or smb", cmd.CommandPath(), protocol)
	}
	a, err := c.Adapters().Get(cmd.Context(), protocol)
	if err != nil {
		return 0, fmt.Errorf("%s: the %s adapter: %w", cmd.CommandPath(), protocol, err)
	}
	if raw, ok := a.Spec.Settings["port"]; ok {
		if err := json.Unmarshal(raw, &port); err != nil {
			return 0, fmt.Errorf("%s: the %s adapter's port %s: %w", cmd.CommandPath(), protocol, raw, err)
		}
	}
	return port, nil
}

// mount is what a mount command is built from: the host's operating
// system, the protocol, the server's host and the adapter's port, the
// share, its NFS path or its SMB name, the mount point, the NFS version,
// the SMB user, and, when dfsctl runs as root under sudo, the invoking
// user's name, uid and gid, whom an SMB mount is made for.
type mount struct {
	GOOS, Protocol, Host string
	Port                 int
	Share, Point         string
	NFSVersion, User     string
	SudoUser, UID, GID   string
}

// mountLine is the command line that mounts m on m.GOOS. No line carries
// a password: an SMB mount asks for it itself.
func mountLine(m mount) ([]string, error) {
	host := m.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	switch m.Protocol + "/" + m.GOOS {
	case "nfs/linux", "nfs/darwin":
		return []string{"mount", "-t", "nfs", "-o", nfsOptions(m), host + ":" + m.Share, m.Point}, nil
	case "nfs/windows":
		// The Windows NFS client mounts only from the standard port.
		if m.Port != 2049 {
			return nil, fmt.Errorf("the Windows NFS client mounts only from port 2049, and the NFS adapter serves %d", m.Port)
		}
		return []string{"mount", "-o", "mtype=hard,nolock", `\\` + m.Host + strings.ReplaceAll(m.Share, "/", `\`), m.Point}, nil
	case "smb/linux", "smb/darwin", "smb/windows":
		if m.User == "" {
			return nil, fmt.Errorf("an SMB mount needs a user: name it with --username")
		}
	default:
		return nil, fmt.Errorf("mounting over %q on %s is not supported", m.Protocol, m.GOOS)
	}
	switch m.GOOS {
	case "linux":
		// mount.cifs splits its options on commas.
		if strings.ContainsAny(m.User, ",=") {
			return nil, fmt.Errorf("an SMB user for mount.cifs holds no comma or equals sign: %q", m.User)
		}
		opts := "vers=2.1,port=" + strconv.Itoa(m.Port) + ",username=" + m.User
		if m.UID != "" {
			opts += ",uid=" + m.UID + ",gid=" + m.GID
		}
		return []string{"mount", "-t", "cifs", "-o", opts, "//" + host + "/" + m.Share, m.Point}, nil
	case "darwin":
		u := url.URL{Scheme: "smb", User: url.User(m.User), Host: host + ":" + strconv.Itoa(m.Port), Path: "/" + m.Share}
		line := []string{"mount_smbfs", u.String(), m.Point}
		// macOS lets only a mount's owner reach its files, so a mount made
		// under sudo is made as the user who ran sudo.
		if m.SudoUser != "" {
			line = append([]string{"sudo", "-u", m.SudoUser}, line...)
		}
		return line, nil
	}
	line := []string{"net", "use", m.Point, `\\` + m.Host + `\` + m.Share, "/user:" + m.User, "*"}
	if m.Port != 445 {
		line = append(line, "/TCPPORT:"+strconv.Itoa(m.Port))
	}
	return line, nil
}

// nfsOptions is an NFS mount's -o: NFSv3 names the MOUNT protocol's port,
// the adapter's own, and on Linux takes nolock, since its locking needs a
// portmapper; NFSv4 carries both in its protocol. actimeo=0 shows a change
// at once, and macOS needs resvport.
func nfsOptions(m mount) string {
	opts := "nfsvers=" + m.NFSVersion + ",tcp,port=" + strconv.Itoa(m.Port)
	if !strings.HasPrefix(m.NFSVersion, "4") {
		opts += ",mountport=" + strconv.Itoa(m.Port)
	}
	opts += ",actimeo=0"
	switch {
	case m.GOOS == "darwin":
		opts += ",resvport"
	case !strings.HasPrefix(m.NFSVersion, "4"):
		opts += ",nolock"
	}
	return opts
}
