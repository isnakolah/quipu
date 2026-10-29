package main

// Reads that page past GitHub's 100-item limit. Repositories that adopt
// Quipu with an imported backlog easily have hundreds of Issues.

import (
	"encoding/json"
	"fmt"

	"github.com/isnakolah/quipu/internal/model"
)

// runGraphQL runs one GraphQL query; tests replace it.
var runGraphQL = func(query string) ([]byte, error) {
	return ghOutput("api", "graphql", "-f", "query="+query)
}

// issuePages runs an Issues query page by page and returns every node in one
// response shaped like an unpaged one, so callers decode it as before.
// filter holds the issues() arguments other than paging, such as
// "states:OPEN"; fields is the node selection.
func issuePages(c model.Config, filter, fields string) ([]byte, error) {
	nodes := []json.RawMessage{}
	after := ""
	for {
		query := fmt.Sprintf("query { repository(owner:%q,name:%q) { issues(first:100%s,%s) { pageInfo { hasNextPage endCursor } nodes {\n%s\n} } } }",
			c.Owner, c.Repository, after, filter, fields)
		output, err := runGraphQL(query)
		if err != nil {
			return nil, err
		}
		var page struct {
			Data struct {
				Repository struct {
					Issues struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []json.RawMessage `json:"nodes"`
					} `json:"issues"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := json.Unmarshal(output, &page); err != nil {
			return nil, err
		}
		issues := page.Data.Repository.Issues
		nodes = append(nodes, issues.Nodes...)
		if !issues.PageInfo.HasNextPage || issues.PageInfo.EndCursor == "" {
			break
		}
		after = fmt.Sprintf(",after:%q", issues.PageInfo.EndCursor)
	}
	return json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"issues": map[string]any{"nodes": nodes}}}})
}
