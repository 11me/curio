package hn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_TopStories(t *testing.T) {
	items := map[string]string{
		"/topstories.json": `[3, 1, 2, 4, 5]`,
		"/item/1.json":     `{"id":1,"type":"story","by":"a","time":1763550000,"title":"One","url":"https://example.com/one","score":120,"descendants":45}`,
		"/item/2.json":     `{"id":2,"type":"story","by":"b","time":1763550000,"title":"Ask HN: Two?","text":"Hi &amp; <i>bye</i><p>next","score":5}`,
		"/item/3.json":     `{"id":3,"type":"story","by":"c","time":1763550000,"title":"Three","url":"https://example.com/3","score":7,"descendants":1}`,
		"/item/4.json":     `{"id":4,"type":"job","title":"Hiring"}`,
		"/item/5.json":     `{"id":5,"type":"story","dead":true,"title":"Dead"}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := items[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewClient(5 * time.Second)
	c.BaseURL = srv.URL
	stories, err := c.TopStories(context.Background(), 4)
	require.NoError(t, err)
	require.Len(t, stories, 3)

	assert.Equal(t, int64(3), stories[0].ID)
	assert.Equal(t, 1, stories[0].Rank)
	assert.Equal(t, int64(1), stories[1].ID)
	assert.Equal(t, 120, stories[1].Points)
	assert.Equal(t, 45, stories[1].Comments)
	assert.Equal(t, "https://example.com/one", stories[1].URL)

	ask := stories[2]
	assert.Equal(t, DiscussionURL(2), ask.URL, "text posts link to the discussion")
	assert.Equal(t, "Hi & bye\n\nnext", ask.Text)
	assert.Equal(t, 3, ask.Rank)
}

func TestClient_TopStoriesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := NewClient(time.Second)
	c.BaseURL = srv.URL
	_, err := c.TopStories(context.Background(), 30)
	require.Error(t, err)
}

func TestClient_ItemRejectsUnsafeURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":9,"type":"story","title":"X","url":"javascript:alert(1)"}`))
	}))
	defer srv.Close()
	c := NewClient(time.Second)
	c.BaseURL = srv.URL
	st, err := c.Item(context.Background(), 9)
	require.NoError(t, err)
	assert.Equal(t, DiscussionURL(9), st.URL)
}
