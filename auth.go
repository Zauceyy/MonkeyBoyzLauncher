package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

var (
	msDeviceURL = "https://login.microsoftonline.com/consumers/oauth2/v2.0/devicecode"
	msTokenURL  = "https://login.microsoftonline.com/consumers/oauth2/v2.0/token"
	xblURL      = "https://user.auth.xboxlive.com/user/authenticate"
	xstsURL     = "https://xsts.auth.xboxlive.com/xsts/authorize"
	mcLoginURL  = "https://api.minecraftservices.com/authentication/login_with_xbox"
	mcProfile   = "https://api.minecraftservices.com/minecraft/profile"
)

const msScope = "XboxLive.signin offline_access"

// builtinMSClientID can be set at build time so every copy of the exe signs in without setup:
//
//	go build -ldflags "-X main.builtinMSClientID=<your approved Azure client id>"
var builtinMSClientID = ""

var errNeedClientID = errors.New("need_client_id")

// msClientID finds the Azure application id: Settings, then ms_client_id.txt next to the exe, then the built-in one.
func msClientID(cfg *Config) string {
	if id := strings.TrimSpace(cfg.MSClientID); id != "" {
		return id
	}
	if id := sideFile("ms_client_id.txt"); id != "" {
		return id
	}
	return builtinMSClientID
}

// offlineUUID matches the UUID a vanilla server gives offline players.
func offlineUUID(name string) string {
	d := md5.Sum([]byte("OfflinePlayer:" + name))
	d[6] = (d[6] & 0x0f) | 0x30
	d[8] = (d[8] & 0x3f) | 0x80
	return hex.EncodeToString(d[:])
}

func offlineAccount(name string) (Account, error) {
	name = strings.TrimSpace(name)
	ok := len(name) >= 3 && len(name) <= 16
	for _, c := range name {
		if !(c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			ok = false
		}
	}
	if !ok {
		return Account{}, errors.New("player names are 3-16 characters: letters, numbers and underscore")
	}
	return Account{Type: "offline", Name: name, UUID: offlineUUID(name), AccessToken: "0"}, nil
}

// postForm returns the JSON body even for 4xx replies, which is how OAuth reports its state.
func postForm(u string, form map[string]string) (M, error) {
	vals := url.Values{}
	for k, v := range form {
		vals.Set(k, v)
	}
	resp, err := httpDo("POST", u, []byte(vals.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out M
	if json.Unmarshal(b, &out) != nil {
		return nil, fmt.Errorf("Microsoft sign-in failed (HTTP %d)", resp.StatusCode)
	}
	return out, nil
}

type deviceLogin struct {
	UserCode   string
	URL        string
	deviceCode string
	interval   int
	expires    time.Time
	clientID   string
}

func startDeviceLogin(clientID string) (*deviceLogin, error) {
	if clientID == "" {
		return nil, errNeedClientID
	}
	r, err := postForm(msDeviceURL, map[string]string{"client_id": clientID, "scope": msScope})
	if err != nil {
		return nil, err
	}
	if asS(r["device_code"]) == "" {
		msg := asS(r["error_description"])
		if msg == "" {
			msg = "Microsoft did not accept the client ID"
		}
		return nil, errors.New(strings.SplitN(msg, "\r\n", 2)[0])
	}
	d := &deviceLogin{UserCode: asS(r["user_code"]), URL: asS(r["verification_uri"]), deviceCode: asS(r["device_code"]),
		interval: asInt(r["interval"]), expires: time.Now().Add(time.Duration(asInt(r["expires_in"])) * time.Second), clientID: clientID}
	if d.interval <= 0 {
		d.interval = 5
	}
	if d.URL == "" {
		d.URL = "https://www.microsoft.com/link"
	}
	return d, nil
}

var pollUnit = time.Second

// finishDeviceLogin waits for the user to approve the sign-in in their browser.
func finishDeviceLogin(d *deviceLogin, cancel <-chan struct{}) (Account, error) {
	interval := d.interval
	for time.Now().Before(d.expires) {
		select {
		case <-cancel:
			return Account{}, errors.New("sign-in cancelled")
		case <-time.After(time.Duration(interval) * pollUnit):
		}
		r, err := postForm(msTokenURL, map[string]string{"grant_type": "urn:ietf:params:oauth:grant-type:device_code",
			"client_id": d.clientID, "device_code": d.deviceCode})
		if err != nil {
			continue
		}
		if asS(r["access_token"]) != "" {
			return minecraftLogin(d.clientID, r)
		}
		switch asS(r["error"]) {
		case "authorization_pending":
		case "slow_down":
			interval += 5
		default:
			msg := asS(r["error_description"])
			if msg == "" {
				msg = "sign-in failed"
			}
			return Account{}, errors.New(strings.SplitN(msg, "\r\n", 2)[0])
		}
	}
	return Account{}, errors.New("sign-in timed out, try again")
}

// refreshAccount renews a Microsoft account whose Minecraft token is close to expiring.
func refreshAccount(a Account) (Account, error) {
	if a.Type != "microsoft" || a.ExpiresAt-300 > time.Now().Unix() {
		return a, nil
	}
	r, err := postForm(msTokenURL, map[string]string{"grant_type": "refresh_token", "client_id": a.ClientID,
		"refresh_token": a.RefreshToken, "scope": msScope})
	if err != nil {
		return a, fmt.Errorf("could not reach Microsoft to refresh your sign-in: %v", err)
	}
	if asS(r["access_token"]) == "" {
		return a, errors.New("your Microsoft sign-in expired. Open Accounts and sign in again")
	}
	return minecraftLogin(a.ClientID, r)
}

func minecraftLogin(clientID string, ms M) (Account, error) {
	fail := func(step string, err error) (Account, error) {
		hint := ""
		var he *HTTPError
		if errors.As(err, &he) {
			switch {
			case step == "Minecraft" && he.Status == 403:
				hint = " This usually means the Azure client ID has not been approved by Mojang yet."
			case step == "profile" && he.Status == 404:
				hint = " This Microsoft account does not own Minecraft: Java Edition."
			case step == "Xbox":
				hint = " The account may have no Xbox profile, or it is a child account that needs a parent's approval."
			}
		}
		return Account{}, fmt.Errorf("Microsoft sign-in worked, but the %s step failed.%s (%v)", step, hint, err)
	}
	xbl, err := postJSON(xblURL, M{"Properties": M{"AuthMethod": "RPS", "SiteName": "user.auth.xboxlive.com",
		"RpsTicket": "d=" + asS(ms["access_token"])}, "RelyingParty": "http://auth.xboxlive.com", "TokenType": "JWT"}, nil)
	if err != nil {
		return fail("Xbox", err)
	}
	xsts, err := postJSON(xstsURL, M{"Properties": M{"SandboxId": "RETAIL", "UserTokens": []string{asS(asM(xbl)["Token"])}},
		"RelyingParty": "rp://api.minecraftservices.com/", "TokenType": "JWT"}, nil)
	if err != nil {
		return fail("Xbox", err)
	}
	xui := asA(asM(asM(xsts)["DisplayClaims"])["xui"])
	if len(xui) == 0 {
		return fail("Xbox", errors.New("no Xbox user in reply"))
	}
	uhs := asS(asM(xui[0])["uhs"])
	mc, err := postJSON(mcLoginURL, M{"identityToken": "XBL3.0 x=" + uhs + ";" + asS(asM(xsts)["Token"])}, nil)
	if err != nil {
		return fail("Minecraft", err)
	}
	token := asS(asM(mc)["access_token"])
	profile, err := getJSON(mcProfile, map[string]string{"Authorization": "Bearer " + token})
	if err != nil {
		return fail("profile", err)
	}
	expires := asInt(asM(mc)["expires_in"])
	if expires == 0 {
		expires = 86400
	}
	return Account{Type: "microsoft", Name: asS(asM(profile)["name"]), UUID: asS(asM(profile)["id"]), AccessToken: token,
		ExpiresAt: time.Now().Unix() + int64(expires), RefreshToken: asS(ms["refresh_token"]), ClientID: clientID}, nil
}
