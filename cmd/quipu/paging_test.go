package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestIssuePagesFollowsCursors(t *testing.T) {
	var queries []string
	defer func(old func(string) ([]byte, error)) { runGraphQL = old }(runGraphQL)
	runGraphQL = func(query string) ([]byte, error) {
		queries = append(queries, query)
		page := len(queries)
		var nodes []string
		count := 100
		if page == 3 {
			count = 50
		}
		for i := 0; i < count; i++ {
			nodes = append(nodes, fmt.Sprintf(`{"number":%d}`, (page-1)*100+i+1))
		}
		return []byte(fmt.Sprintf(`{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":%v,"endCursor":"c%d"},"nodes":[%s]}}}}`,
			page < 3, page, strings.Join(nodes, ","))), nil
	}
	raw, err := issuePages(watchConfig(), "states:OPEN", "number")
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Data struct {
			Repository struct {
				Issues struct {
					Nodes []struct {
						Number int `json:"number"`
					} `json:"nodes"`
				} `json:"issues"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	nodes := response.Data.Repository.Issues.Nodes
	if len(nodes) != 250 || nodes[249].Number != 250 {
		t.Fatalf("got %d nodes", len(nodes))
	}
	if len(queries) != 3 || strings.Contains(queries[0], "after:") || !strings.Contains(queries[1], `after:"c1"`) || !strings.Contains(queries[2], `after:"c2"`) {
		t.Fatalf("queries=%v", queries)
	}
	if !strings.Contains(queries[0], "issues(first:100,states:OPEN)") {
		t.Fatalf("filter: %s", queries[0])
	}
}

func TestDrainWindowReachesBackToLastRun(t *testing.T) {
	now := time.Date(2026, 10, 29, 18, 0, 0, 0, time.UTC)
	if got := drainWindow(now.Add(-5*time.Hour), now); got != 5*time.Hour+drainMargin {
		t.Fatalf("slept runner: %v", got)
	}
	if got := drainWindow(time.Time{}, now); got != 0 {
		t.Fatalf("no previous run scans everything: %v", got)
	}
	if got := drainWindow(now.Add(time.Hour), now); got != 0 {
		t.Fatalf("clock skew into the future scans everything: %v", got)
	}
}
