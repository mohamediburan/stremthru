package kizaru

import (
	"net/http"

	"github.com/MunifTanjim/stremthru/store"
)

// GetUserData is Kizaru's access-pass payload. A null body means the user holds
// no pass at all.
type GetUserData struct {
	Id     string `json:"id"`
	UserId string `json:"userId"`
	Plan   string `json:"plan"`
	Status string `json:"status"` // "active" | "expired"
}

type GetUserParams struct {
	Ctx
}

func (c APIClient) GetUser(params *GetUserParams) (APIResponse[*GetUserData], error) {
	response := &Response[*GetUserData]{}
	res, err := c.Request(http.MethodGet, "/api/v1/access", params, response)
	return newAPIResponse(res, response.Data), err
}

// subscriptionStatus maps Kizaru's access-pass status onto StremThru's.
//
// Kizaru reports a binary live/lapsed window, so anything that is not "active"
// maps to expired: answering premium without a live pass would hand out paid
// bandwidth for free.
func subscriptionStatus(pass *GetUserData) store.UserSubscriptionStatus {
	if pass != nil && pass.Status == "active" {
		return store.UserSubscriptionStatusPremium
	}
	return store.UserSubscriptionStatusExpired
}
