package isureal

import (
	"sort"
	"testing"
	"time"
)

// postAccess.viewer が作者の分だけ持つ viewerContext で、全体を持つ viewerContext と同じ判定になることを確かめる。
func TestPostAccessViewerMatchesFullViewerContext(t *testing.T) {
	const viewerID, authorID, otherID = "viewer", "author", "other"
	ended := time.Now()
	for _, authorIsViewer := range []bool{false, true} {
		for _, windowEnded := range []bool{false, true} {
			for _, posted := range []bool{false, true} {
				for _, blocked := range []bool{false, true} {
					for _, friend := range []bool{false, true} {
						for _, friendOfFriend := range []bool{false, true} {
							for _, visibility := range []string{"public", "friends", "friends_of_friends", "unknown"} {
								post := Post{ID: "post", AuthorID: authorID, Visibility: visibility}
								if authorIsViewer {
									post.AuthorID = viewerID
								}
								if windowEnded {
									post.WindowEndedAt = &ended
								}
								full := viewerContext{
									User: User{ID: viewerID}, HasPostedToday: posted,
									Friends:          map[string]bool{otherID: true},
									FriendsOfFriends: map[string]bool{otherID: true},
									MutualFriends:    map[string]int{otherID: 1},
									Blocked:          map[string]bool{otherID: true},
								}
								if blocked {
									full.Blocked[post.AuthorID] = true
								}
								if friend {
									full.Friends[post.AuthorID] = true
								}
								if friendOfFriend {
									full.FriendsOfFriends[post.AuthorID] = true
								}
								access := postAccess{
									PostWithAuthor: PostWithAuthor{Post: post}, ViewerActive: true,
									ViewerHasPostedToday: posted, Blocked: blocked, Friend: friend, FriendOfFriend: friendOfFriend,
								}
								if got, want := access.canView(viewerID), full.canView(post); got != want {
									t.Errorf("canView mismatch: post=%+v posted=%v blocked=%v friend=%v fof=%v: got %v want %v",
										post, posted, blocked, friend, friendOfFriend, got, want)
								}
								access.ViewerActive = false
								if access.canView(viewerID) {
									t.Errorf("inactive viewer must not view: post=%+v", post)
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestMapKeys(t *testing.T) {
	keys := mapKeys(map[string]bool{"a": true, "b": false}, map[string]bool{"a": true, "c": true})
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "a" || keys[1] != "c" {
		t.Errorf("mapKeys = %v", keys)
	}
	if keys := mapKeys(); keys == nil || len(keys) != 0 {
		t.Errorf("mapKeys() must be an empty non-nil slice, got %#v", keys)
	}
}
