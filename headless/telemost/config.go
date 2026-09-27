package main

import (
	"fmt"
	"log"
	"regexp"

	"whitelist-bypass/relay/common"
)

type TMConfig struct {
	AppVersion string
	SDKVersion string
}

// fetchConfig fetches live version info from the Telemost/telemessenger frontend.
//
// Since Telemost migrated to the Yandex Messenger (telemessenger) platform the
// old parsing strategy (preloaded-state JSON + telemost.yastatic.net bundle URL)
// no longer works.  The new approach:
//
//  1. Fetch telemost.yandex.ru main page.
//  2. Extract the telemessenger build version from the CDN path
//     (e.g. "chat-static/telemessenger/_/212.3.0/web/app.js").
//     This version string is used for both AppVersion and SDKVersion.
//  3. Fall back to sensible hard-coded defaults so a single CDN bump does not
//     break the creator.
func fetchConfig() (TMConfig, error) {
	var cfg TMConfig

	page, err := common.HttpGet("https://telemost.yandex.ru/")
	if err != nil {
		return cfg, fmt.Errorf("failed to fetch telemost.yandex.ru: %w", err)
	}

	// Pattern: //yastatic.net/s3/chat-static/telemessenger/_/212.3.0/web/app.js
	versionRe := regexp.MustCompile(`chat-static/telemessenger/_/(\d+\.\d+\.\d+)/web/`)
	if m := versionRe.FindSubmatch(page); m != nil {
		ver := string(m[1])
		cfg.AppVersion = ver
		cfg.SDKVersion = "6.2.1"
		log.Printf("[config] telemessenger version=%s (app=%s sdk=%s)", ver, cfg.AppVersion, cfg.SDKVersion)
		return cfg, nil
	}

	// Legacy fallback: try the old preloaded-state approach.
	log.Println("[config] telemessenger version pattern not found, trying legacy preloaded-state")
	stateRe := regexp.MustCompile(`<script[^>]*id="preloaded-state"[^>]*>([\s\S]*?)</script>`)
	if stateMatch := stateRe.FindSubmatch(page); stateMatch != nil {
		avRe := regexp.MustCompile(`"appVersion"\s*:\s*"([^"]+)"`)
		if av := avRe.FindSubmatch(stateMatch[1]); av != nil {
			cfg.AppVersion = string(av[1])
		}
	}

	if cfg.AppVersion == "" {
		// Hard-coded fallback -- update when the server starts rejecting this.
		cfg.AppVersion = "212.3.0"
		cfg.SDKVersion = "6.2.1"
		log.Printf("[config] using hard-coded fallback version app=%s sdk=%s", cfg.AppVersion, cfg.SDKVersion)
		return cfg, nil
	}

	cfg.SDKVersion = "6.2.1"
	log.Printf("[config] legacy fallback app=%s sdk=%s", cfg.AppVersion, cfg.SDKVersion)
	return cfg, nil
}
