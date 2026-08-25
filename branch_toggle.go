package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var (
	branchToggleMu   sync.RWMutex
	disabledBranches = make(map[string]bool)
	branchTogglePath string
)

func initBranchToggle(dir string) {
	branchToggleMu.Lock()
	defer branchToggleMu.Unlock()
	branchTogglePath = filepath.Join(dir, "disabled_branches.json")
	blob, err := os.ReadFile(branchTogglePath)
	if err == nil {
		var list []string
		if err := json.Unmarshal(blob, &list); err == nil {
			for _, k := range list {
				disabledBranches[k] = true
			}
		}
	}
}

func branchKey(tag string, port int) string {
	return fmt.Sprintf("%s:%d", tag, port)
}

func isBranchEnabled(tag string, port int) bool {
	branchToggleMu.RLock()
	defer branchToggleMu.RUnlock()
	return !disabledBranches[branchKey(tag, port)]
}

func setBranchEnabled(tag string, port int, enabled bool) error {
	branchToggleMu.Lock()
	defer branchToggleMu.Unlock()
	k := branchKey(tag, port)
	if enabled {
		delete(disabledBranches, k)
	} else {
		disabledBranches[k] = true
	}

	var list []string
	for key := range disabledBranches {
		list = append(list, key)
	}
	blob, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if branchTogglePath == "" {
		return nil
	}
	tmp := branchTogglePath + ".tmp"
	if err := os.WriteFile(tmp, blob, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, branchTogglePath)
}
