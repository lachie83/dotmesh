package commands

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/dotmesh-io/dotmesh/cmd/dm/pkg/remotes"
	"github.com/spf13/cobra"
)

func NewCmdDotSetUpstream(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-upstream",
		Short: "Set or update the default dot on a remote",
		Long:  "Online help: https://docs.dotmesh.com/references/cli/#set-the-upstream-dot-dm-dot-set-upstream-dot-remote-remote-dot",

		Run: func(cmd *cobra.Command, args []string) {
			err := dotSetUpstream(cmd, args, out)
			if err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				os.Exit(1)
			}
		},
	}
	return cmd
}

func NewCmdDotShow(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Display information about a dot",
		Long:  "Online help: https://docs.dotmesh.com/references/cli/#examine-a-dot-dm-dot-show-h-scripting-dot",

		Run: func(cmd *cobra.Command, args []string) {
			err := dotShow(cmd, args, out)
			if err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				os.Exit(1)
			}
		},
	}
	cmd.Flags().BoolVarP(
		&scriptingMode, "scripting", "H", false,
		"scripting mode. Do not print headers, separate fields by "+
			"a single tab instead of arbitrary whitespace.",
	)
	return cmd
}

func NewCmdDotDelete(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a dot",
		Long:  "Online help: https://docs.dotmesh.com/references/cli/#delete-a-dot-dm-dot-delete-f-force-dot",

		Run: func(cmd *cobra.Command, args []string) {
			err := dotDelete(cmd, args, out)
			if err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				os.Exit(1)
			}
		},
	}
	cmd.Flags().BoolVarP(
		&forceMode, "force", "f", false,
		"perform dangerous operations without requiring confirmation.",
	)
	return cmd
}

func NewCmdDot(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dot",
		Short: `Manage dots`,
		Long: `Manage dots in the cluster.

Run 'dm dot set-upstream [<dot>] <remote> <remote-dot>' to
change the default remote dot for <dot> on <remote>.

Run 'dm dot show [<dot>]' to show information about the dot.

Where '[<dot>]' is omitted, the current dot (selected by 'dm switch')
is used.`,
	}

	cmd.AddCommand(NewCmdDotSetUpstream(os.Stdout))
	cmd.AddCommand(NewCmdDotShow(os.Stdout))
	cmd.AddCommand(NewCmdDotDelete(os.Stdout))

	return cmd
}

func dotSetUpstream(cmd *cobra.Command, args []string, out io.Writer) error {
	dm, err := remotes.NewDotmeshAPI(configPath)
	if err != nil {
		return err
	}

	var localDot, peer, remoteDot string

	switch len(args) {
	case 2:
		localDot, err = dm.CurrentVolume()
		if err != nil {
			return err
		}

		peer = args[0]
		remoteDot = args[1]
	case 3:
		localDot = args[0]
		peer = args[1]
		remoteDot = args[2]
	default:
		return fmt.Errorf("Please specify [<dot>] <remote> <remote-dot> as arguments.")
	}

	remote, err := dm.Configuration.GetRemote(peer)
	if err != nil {
		return err
	}

	localNamespace, localDot, err := remotes.ParseNamespacedVolume(localDot)
	if err != nil {
		return err
	}

	remoteNamespace, remoteDot, err := remotes.ParseNamespacedVolumeWithDefault(remoteDot, remote.User)
	if err != nil {
		return err
	}

	dm.Configuration.SetDefaultRemoteVolumeFor(peer, localNamespace, localDot, remoteNamespace, remoteDot)
	return nil
}

func dotDelete(cmd *cobra.Command, args []string, out io.Writer) error {
	dm, err := remotes.NewDotmeshAPI(configPath)
	if err != nil {
		return err
	}

	var dot string
	switch len(args) {
	case 1:
		dot = args[0]
	default:
		return fmt.Errorf("Please specify the dot to delete (the default dot is ignored for deletion, to avoid mistakes).")
	}

	if !forceMode {
		fmt.Printf("Please confirm that you really want to delete the dot %s, including all branches and commits? (enter Y to continue): ", dot)
		reader := bufio.NewReader(os.Stdin)
		text, _ := reader.ReadString('\n')
		if text != "Y\n" {
			fmt.Printf("Aborted.\n")
			return nil
		}
	}

	err = dm.DeleteVolume(dot)
	if err != nil {
		return err
	}

	return nil
}

func dotShow(cmd *cobra.Command, args []string, out io.Writer) error {
	dm, err := remotes.NewDotmeshAPI(configPath)
	if err != nil {
		return err
	}

	var localDot string
	if len(args) == 1 {
		localDot = args[0]
	} else {
		localDot, err = dm.CurrentVolume()
		if err != nil {
			return err
		}
	}

	namespace, dot, err := remotes.ParseNamespacedVolume(localDot)
	if err != nil {
		return err
	}
	if scriptingMode {
		fmt.Fprintf(out, "namespace\t%s\n", namespace)
		fmt.Fprintf(out, "name\t%s\n", dot)
	} else {
		fmt.Fprintf(out, "Dot %s/%s:\n", namespace, dot)
	}

	var dotmeshDot *remotes.DotmeshVolume
	vs, err := dm.AllVolumes()
	if err != nil {
		return err
	}

	for _, v := range vs {
		if v.Name.Namespace == namespace && v.Name.Name == dot {
			dotmeshDot = &v
			break
		}
	}
	if dotmeshDot == nil {
		return fmt.Errorf("Unable to find dot '%s'", localDot)
	}

	if scriptingMode {
		fmt.Fprintf(out, "masterBranchId\t%s\n", dotmeshDot.Id)
	} else {
		fmt.Fprintf(out, "Master branch ID: %s\n", dotmeshDot.Id)
	}

	activeQualified, err := dm.CurrentVolume()
	if err != nil {
		return err
	}
	activeNamespace, activeDot, err := remotes.ParseNamespacedVolume(activeQualified)
	if err != nil {
		return err
	}

	if namespace == activeNamespace && dot == activeDot {
		if scriptingMode {
			fmt.Fprintf(out, "current\n")
		} else {
			fmt.Fprintf(out, "Dot is current.\n")
		}
	}

	if scriptingMode {
		fmt.Fprintf(out, "size\t%d\ndirty\t%d\n",
			dotmeshDot.SizeBytes,
			dotmeshDot.DirtyBytes)
	} else {
		if dotmeshDot.DirtyBytes == 0 {
			fmt.Fprintf(out, "Dot size: %s (all clean)\n", prettyPrintSize(dotmeshDot.SizeBytes))
		} else {
			fmt.Fprintf(out, "Dot size: %s (%s dirty)\n",
				prettyPrintSize(dotmeshDot.SizeBytes),
				prettyPrintSize(dotmeshDot.DirtyBytes))
		}
	}

	currentBranch, err := dm.CurrentBranch(localDot)
	if err != nil {
		return err
	}

	bs, err := dm.AllBranches(localDot)
	if err != nil {
		return err
	}

	if !scriptingMode {
		fmt.Fprintf(out, "Branches:\n")
	} else {
		fmt.Fprintf(out, "currentBranch\t%s\n", currentBranch)
	}

	for _, branch := range bs {
		if !scriptingMode {
			if branch == currentBranch {
				branch = "* " + branch
			} else {
				branch = "  " + branch
			}
		}

		containerNames := []string{}

		if branch == currentBranch {
			containerInfo, err := dm.RelatedContainers(dotmeshDot.Name, branch)
			if err != nil {
				return err
			}
			for _, container := range containerInfo {
				containerNames = append(containerNames, container.Name)
			}
		}

		if scriptingMode {
			fmt.Fprintf(out, "branch\t%s\n", branch)
			for _, c := range containerNames {
				fmt.Fprintf(out, "container\t%s\t%s\n", branch, c)
			}
		} else {
			if len(containerNames) == 0 {
				fmt.Fprintf(out, "%s\n", branch)
			} else {
				fmt.Fprintf(out, "%s (containers: %s)\n", branch, strings.Join(containerNames, ","))
			}
		}
	}

	remotes := dm.Configuration.GetRemotes()
	keys := []string{}
	// sort the keys so we can iterate over in human friendly order
	for k, _ := range remotes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		remoteNamespace, remoteDot, ok := dm.Configuration.DefaultRemoteVolumeFor(k, namespace, dot)
		if ok {
			if scriptingMode {
				fmt.Fprintf(out, "defaultUpstreamDot\t%s\t%s/%s\n",
					k,
					remoteNamespace,
					remoteDot)
			} else {
				fmt.Fprintf(out, "Tracks dot %s/%s on remote %s\n", remoteNamespace, remoteDot, k)
			}
		}
	}
	return nil
}
