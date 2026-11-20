package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
)

func TestCaptchaRemainsInMemoryForAllPromptResults(t *testing.T) {
	temp := t.TempDir()
	if os.PathSeparator == '/' {
		t.Setenv("TMPDIR", temp)
	}
	t.Setenv("TEMP", temp)
	for _, outcome := range []struct {
		name      string
		answer    string
		promptErr error
		status    int
	}{
		{"success", "expected", nil, http.StatusFound},
		{"rejected", "wrong", nil, http.StatusOK},
		{"cancelled", "", context.Canceled, 0},
		{"timed out", "", context.DeadlineExceeded, 0},
	} {
		t.Run(outcome.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/core/auth/captcha/image/current/" {
					if _, err := r.Cookie("jms_sessionid"); err != nil {
						t.Error("CAPTCHA image did not use the login session")
					}
					if r.Header.Get("Cache-Control") != "no-store" {
						t.Error("image request allows caching")
					}
					w.Header().Set("Content-Type", "image/png")
					_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nfixture"))
					return
				}
				if r.Method == http.MethodGet {
					http.SetCookie(w, &http.Cookie{Name: "jms_sessionid", Value: "fixture", Path: "/"})
					fmt.Fprint(w, `<input name="csrfmiddlewaretoken" value="csrf"><img class="captcha" src="/core/auth/captcha/image/current/"><input name="captcha_0" value="captcha-key"><input name="captcha_1">`)
					return
				}
				if r.FormValue("captcha_1") != outcome.answer || r.FormValue("captcha_0") != "captcha-key" {
					t.Error("CAPTCHA answer did not match the displayed challenge")
				}
				if outcome.status == http.StatusFound {
					http.Redirect(w, r, "/ui/", http.StatusFound)
					return
				}
				if outcome.status == 0 {
					t.Error("cancelled prompt submitted a login")
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			client, err := newClientWithAuth(server.URL, "", "")
			if err != nil {
				t.Fatal(err)
			}
			client.inlineCaptcha = true
			checkTemp := func() {
				entries, err := os.ReadDir(temp)
				if err != nil || len(entries) != 0 {
					t.Fatalf("CAPTCHA wrote a local file: %v %v", entries, err)
				}
			}
			err = client.webLogin(context.Background(), "tester", "test", func(_ context.Context, question userQuestion) (string, error) {
				if !question.echo {
					t.Error("CAPTCHA input should be visible")
				}
				if question.image != "data:image/png;base64,iVBORw0KGgpmaXh0dXJl" {
					t.Error("CAPTCHA image was not delivered inline")
				}
				checkTemp()
				return outcome.answer, outcome.promptErr
			})
			if (outcome.status == http.StatusFound) != (err == nil) || (outcome.promptErr != nil && !errors.Is(err, outcome.promptErr)) {
				t.Fatalf("unexpected login result: %v", err)
			}
			checkTemp()
		})
	}
}

func TestOldHostDoesNotOpenExternalCaptcha(t *testing.T) {
	client, err := newClientWithAuth("https://fixture.example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	login, _ := url.Parse("https://fixture.example/core/auth/login/")
	_, err = client.promptCaptcha(context.Background(), login, nil, func(context.Context, userQuestion) (string, error) {
		t.Fatal("old host opened an external CAPTCHA prompt")
		return "", nil
	})
	if err == nil {
		t.Fatal("missing host capability was not reported")
	}
}

func TestInlineChallengeUsesDynamicCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `<input name="csrfmiddlewaretoken" value="masked"><input name="challenge">`)
			return
		}
		if r.FormValue("challenge") != "123456" {
			t.Error("inline dynamic code was not submitted")
		}
		http.Redirect(w, r, "/ui/", http.StatusFound)
	}))
	defer server.Close()
	c, err := newClientWithAuth(server.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	err = c.webLogin(context.Background(), "tester", "test", func(_ context.Context, question userQuestion) (string, error) {
		if question.echo {
			t.Error("dynamic code input should remain masked")
		}
		if question.title != "JumpServer 双因素认证" {
			t.Errorf("unexpected prompt %s", question.title)
		}
		return "123456", nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
