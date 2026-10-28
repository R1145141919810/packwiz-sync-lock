package cmd

import (
	"github.com/spf13/cobra"
)

// ModCmd is the parent command for working with the mod files of a pack: scanning them on
// Modrinth/CurseForge and updating them from those hosts.
var ModCmd = &cobra.Command{
	Use:   "mod",
	Short: "Identify and update mods on Modrinth and CurseForge",
}

func init() {
	rootCmd.AddCommand(ModCmd)
}
