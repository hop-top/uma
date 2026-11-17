package linear

import (
	"context"

	"hop.top/uma/model"
)

// gqlComment is a comment as the comment query returns it.
type gqlComment struct {
	ID         string  `json:"id"`
	Body       string  `json:"body"`
	CreatedAt  string  `json:"createdAt"`
	UpdatedAt  string  `json:"updatedAt"`
	EditedAt   *string `json:"editedAt"`
	ArchivedAt *string `json:"archivedAt"`
	URL        string  `json:"url"`
	IssueID    string  `json:"issueId"`
	ParentID   *string `json:"parentId"`
	User       *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		App  bool   `json:"app"`
	} `json:"user"`
	BotActor *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"botActor"`
	Issue *struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		Title      string `json:"title"`
		URL        string `json:"url"`
		Team       struct {
			Key string `json:"key"`
		} `json:"team"`
	} `json:"issue"`
}

// gqlIssue is an issue as the issue query returns it.
type gqlIssue struct {
	ID          string  `json:"id"`
	Identifier  string  `json:"identifier"`
	Number      float64 `json:"number"`
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Description *string `json:"description"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
	ArchivedAt  *string `json:"archivedAt"`
	Trashed     bool    `json:"trashed"`
	Team        struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	} `json:"team"`
	State struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
	Creator  *gqlUser `json:"creator"`
	Assignee *gqlUser `json:"assignee"`
	Labels   struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

type gqlUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// readComment reads the comment id. gone reports a comment that does not
// exist, or that Linear holds archived (a deleted comment is archived).
func (x *Instance) readComment(ctx context.Context, id string) (c *gqlComment, gone bool, err error) {
	var out struct {
		Comment *gqlComment `json:"comment"`
	}
	a, err := x.exec(ctx, queryComment, map[string]any{"id": id}, &out)
	if err != nil || a.failed() {
		if a != nil && a.notFound() {
			return nil, true, nil
		}
		return nil, false, x.readFailure(ctx, a, err)
	}
	if out.Comment == nil || out.Comment.ID == "" {
		return nil, true, nil
	}
	if out.Comment.ArchivedAt != nil && *out.Comment.ArchivedAt != "" {
		return out.Comment, true, nil
	}
	return out.Comment, false, nil
}

// readIssue reads the issue id (a UUID or an identifier such as ENG-123).
func (x *Instance) readIssue(ctx context.Context, id string) (*gqlIssue, error) {
	var out struct {
		Issue *gqlIssue `json:"issue"`
	}
	a, err := x.exec(ctx, queryIssue, map[string]any{"id": id}, &out)
	if err != nil || a.failed() {
		return nil, x.readFailure(ctx, a, err)
	}
	if out.Issue == nil || out.Issue.ID == "" {
		return nil, final(model.NotFound, "issue not found or inaccessible")
	}
	return out.Issue, nil
}
