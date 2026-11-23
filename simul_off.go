//go:build !simul

package main

import "context"

// Simulwatching is disabled in normal builds (see simul.go, built with
// -tags simul). These no-ops keep the rest of the app unaware of it.

func simulJoined() bool                               { return false }
func simulApplyJoinStateToUI(Config, *discordManager) {}
func simulPublishIdle()                               {}
func simulPublish(playbackSnapshot)                   {}
func simulPublishSync(aniID, episode, totalEps int)   {}
func addSimulMenu(ctx context.Context)                {}
