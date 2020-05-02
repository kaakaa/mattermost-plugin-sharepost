package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/mattermost/mattermost-server/v5/plugin"
	"github.com/pkg/errors"
)

const (
	toChannelKey      = "to_channel"
	shareTypeKey      = "share_type"
	additionalTextKey = "additional_text"

	SHARE_TYPE_SHARE = "share"
	SHARE_TYPE_MOVE  = "move"
)

var messageGenericError = toPtr("Something went wrong. Please try again later.")

type submitDialogHandler func(map[string]string, *model.SubmitDialogRequest) (*string, *model.SubmitDialogResponse, error)

func (p *SharePostPlugin) InitAPI() *mux.Router {
	r := mux.NewRouter()
	r.HandleFunc("/", p.handleInfo).Methods(http.MethodGet)

	apiV1 := r.PathPrefix("/api/v1").Subrouter()
	apiV1.Use(checkAuthenticity)
	apiV1.HandleFunc("/share", p.handleSubmitDialogRequest(p.handleSharePost)).Methods(http.MethodPost)
	// apiV1.HandleFunc("/move", p.handleSubmitDialogRequest(p.handleMovePost).Methods(http.MethodPost)
	return r
}

func (p *SharePostPlugin) ServeHTTP(c *plugin.Context, w http.ResponseWriter, r *http.Request) {
	p.API.LogDebug("New request:", "Host", r.Host, "RequestURI", r.RequestURI, "Method", r.Method)
	p.router.ServeHTTP(w, r)
}

func (p *SharePostPlugin) handleInfo(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, fmt.Sprintf("Installed SharePostPlugin v%s", manifest.Version))
}

func checkAuthenticity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mattermost-User-ID") == "" {
			http.Error(w, "not authorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (p *SharePostPlugin) handleSubmitDialogRequest(handler submitDialogHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		request := model.SubmitDialogRequestFromJson(r.Body)
		if request == nil {
			p.API.LogWarn("Failed to decode SubmitDialogRequest")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		if request.UserId != r.Header.Get("Mattermost-User-Id") {
			p.API.LogWarn("invalid user")
			http.Error(w, "not authorized", http.StatusUnauthorized)
			return
		}

		msg, response, err := handler(mux.Vars(r), request)
		if err != nil {
			p.API.LogWarn("Failed to handle SubmitDialogRequest", "error", err.Error())
		}

		if msg != nil {
			p.SendEphemeralPost(request.ChannelId, request.UserId, *msg)
		}

		if response != nil {
			w.Header().Set("Content-Type", "application/json")
			err = json.NewEncoder(w).Encode(response)
			if err != nil {
				p.API.LogWarn("Failed to write SubmitDialogRequest", "error", err.Error())
				w.WriteHeader(http.StatusInternalServerError)
			}
		}
	}
}

func (p *SharePostPlugin) handleSharePost(vars map[string]string, request *model.SubmitDialogRequest) (*string, *model.SubmitDialogResponse, error) {
	toChannel, ok := request.Submission[toChannelKey].(string)
	if !ok {
		return messageGenericError, nil, errors.Errorf("failed to get toChannel key. Value is: %v", request.Submission[toChannelKey])
	}
	shareType, ok := request.Submission[shareTypeKey].(string)
	if !ok {
		return messageGenericError, nil, errors.Errorf("failed to get shareType key. Value is: %v", request.Submission[shareTypeKey])
	}
	additionalText, ok := request.Submission[additionalTextKey].(string)
	if ok {
		additionalText = fmt.Sprintf("%s\n\n", additionalText)
	}

	switch shareType {
	case SHARE_TYPE_SHARE:
		return p.sharePost(request, toChannel, additionalText)
	case SHARE_TYPE_MOVE:
		return p.movePost(request, toChannel, additionalText)
	default:
		return messageGenericError, nil, fmt.Errorf("Invalid share_type %s", shareType)
	}
}

func (p *SharePostPlugin) sharePost(request *model.SubmitDialogRequest, toChannel, additionalText string) (*string, *model.SubmitDialogResponse, error) {
	postId := request.CallbackId
	teamId := request.TeamId
	team, appErr := p.API.GetTeam(teamId)
	if appErr != nil {
		p.API.LogError("Failed to get team", "team_id", teamId, "error", appErr.Error())
		return messageGenericError, nil, fmt.Errorf("Failed to get team %w", appErr)
	}

	if _, err := p.API.CreatePost(&model.Post{
		Type:      model.POST_DEFAULT,
		UserId:    request.UserId,
		ChannelId: toChannel,
		Message:   fmt.Sprintf("%s> Shared from %s", additionalText, p.makePostLink(team.Name, postId)),
	}); err != nil {
		p.API.LogWarn("Failed to create post", "error", err.Error())
		return messageGenericError, nil, fmt.Errorf("Failed to create post %w", err)
	}
	return nil, nil, nil
}

func (p *SharePostPlugin) movePost(request *model.SubmitDialogRequest, toChannel, additionalText string) (*string, *model.SubmitDialogResponse, error) {
	postId := request.CallbackId
	postList, appErr := p.API.GetPostThread(postId)
	if appErr != nil {
		p.API.LogError("Failed to get post list", "post_id", postId, "error", appErr.Error())
		return messageGenericError, nil, fmt.Errorf("Failed to get post list %w", appErr)
	}
	postList.UniqueOrder()
	// Cannot move post thread to other channel
	if len(postList.Posts) > 2 {
		p.API.LogWarn("The post that has parent or child posts cannot be moved to other channel.", "post_id", postId)
		return toPtr("The post that has parent or child posts cannot be moved to other channel."), nil, nil
	}

	oldPost, appErr := p.API.GetPost(postId)
	if appErr != nil {
		p.API.LogError("Failed to get post", "post_id", postId, "error", appErr.Error())
		return messageGenericError, nil, fmt.Errorf("Failed to get post %w", appErr)
	}

	if oldPost.ChannelId == toChannel {
		p.API.LogWarn("Cannot move the post to same channel.")
		return toPtr("Cannot move the post to same channel."), nil, nil
	}

	teamId := request.TeamId
	team, appErr := p.API.GetTeam(teamId)
	if appErr != nil {
		p.API.LogError("Failed to get team", "team_id", teamId, "error", appErr.Error())
		return messageGenericError, nil, fmt.Errorf("Failed to get team %w", appErr)
	}

	newPost := oldPost.Clone()
	newPost.Id = ""
	newPost.ChannelId = toChannel
	newPost.Message = fmt.Sprintf("%s%s", additionalText, oldPost.Message)

	movedPost, appErr := p.API.CreatePost(newPost)
	if appErr != nil {
		p.API.LogWarn("Failed to create post", "error", appErr.Error())
		return messageGenericError, nil, fmt.Errorf("Failed to create post %w", appErr)
	}
	if appErr := p.API.DeletePost(oldPost.Id); appErr != nil {
		p.API.LogError("Failed to create post", "error", appErr.Error())
		return messageGenericError, nil, fmt.Errorf("Failed to create post %w", appErr)
	}
	p.SendEphemeralPost(oldPost.ChannelId, request.UserId, fmt.Sprintf("This post is moved to %s", p.makePostLink(team.Name, movedPost.Id)))
	return nil, nil, nil
}

func (p *SharePostPlugin) makePostLink(teamName, postId string) string {
	return fmt.Sprintf("%s/%s/pl/%s", *p.ServerConfig.ServiceSettings.SiteURL, teamName, postId)
}

func toPtr(s string) *string {
	return &s
}
