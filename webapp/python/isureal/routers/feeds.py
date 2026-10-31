from collections import Counter
from datetime import datetime
from http import HTTPStatus
from typing import Annotated

from fastapi import APIRouter, Query

from ..auth import RegionalSession
from ..binding import cursor_int, cursor_time
from ..db import DB, Connection, fetch_all
from ..errors import APIError
from ..models.post import Post, Viewer, load_viewer, select_posts_with_author
from ..schemas import FeedPostResponse, Page

router = APIRouter(prefix="/api/feeds")

FEEDS = frozenset({"friends", "friends-of-friends", "public", "following"})


def matches_feed(viewer: Viewer, following: set[str], post: Post, feed: str) -> bool:
    match feed:
        case "public":
            return post.visibility == "public"
        case "following":
            return post.author_id in following
        case "friends":
            return post.author_id in viewer.friends
        case "friends-of-friends":
            return (
                post.author_id not in viewer.friends
                and post.author_id in viewer.friends_of_friends
            )
        case _:
            return False


def ranked_before(
    score: int, post: Post, before_score: int, before_at: datetime, before_id: str
) -> bool:
    if score != before_score:
        return score < before_score
    if post.posted_at != before_at:
        return post.posted_at < before_at
    return post.id < before_id


async def recommendation_scores(
    db: Connection, viewer: Viewer, posts: list[Post]
) -> dict[str, int]:
    if not posts:
        return {}
    friendships = await fetch_all(
        db,
        """
        SELECT user_low_id, user_high_id FROM friendships
        WHERE user_low_id = ANY(%(ids)s) OR user_high_id = ANY(%(ids)s)
        """,
        {"ids": [post.author_id for post in posts]},
    )
    connections: Counter[str] = Counter()
    for friendship in friendships:
        low, high = friendship["user_low_id"], friendship["user_high_id"]
        connections[low] += viewer.mutual_friends[high]
        connections[high] += viewer.mutual_friends[low]
    engagements = await fetch_all(
        db,
        """
        SELECT r.post_id, r.reactor_id AS user_id FROM realmojis r
        JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
        WHERE r.post_id = ANY(%(ids)s) AND r.replaced_at IS NULL AND r.deleted_at IS NULL
        UNION
        SELECT c.post_id, c.author_id AS user_id FROM comments c
        JOIN users u ON u.id = c.author_id AND u.status = 'active'
        WHERE c.post_id = ANY(%(ids)s) AND c.deleted_at IS NULL
        """,
        {"ids": [post.id for post in posts]},
    )
    friend_engagements = Counter(
        row["post_id"] for row in engagements if row["user_id"] in viewer.friends
    )
    return {
        post.id: 10 * friend_engagements[post.id]
        + 3 * viewer.mutual_friends[post.author_id]
        + min(connections[post.author_id], 30)
        for post in posts
    }


@router.get("/{feed}")
async def get_feed(
    db: DB,
    session: RegionalSession,
    feed: str,
    before_score: Annotated[str | None, Query(alias="beforeScore")] = None,
    before_posted_at: Annotated[str | None, Query(alias="beforePostedAt")] = None,
    before_id: Annotated[str, Query(alias="beforeId")] = "",
) -> Page[FeedPostResponse]:
    score_cursor = cursor_int(before_score)
    before_at = cursor_time(before_posted_at)
    if feed not in FEEDS:
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_post")
    if (before_at is None) != (before_id == ""):
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_pagination")
    ranked = feed == "friends-of-friends"
    if ranked and (score_cursor is None) != (before_id == ""):
        raise APIError(HTTPStatus.BAD_REQUEST, "invalid_pagination")
    viewer = await load_viewer(db, session.user_id)
    if viewer is None:
        return Page.of([])
    following: set[str] = set()
    if feed == "following":
        rows = await fetch_all(
            db,
            "SELECT followee_id FROM follows WHERE follower_id = %(viewer)s",
            {"viewer": viewer.id},
        )
        following = {row["followee_id"] for row in rows}
    posts = await select_posts_with_author(
        db,
        """
        AND p.author_id <> %(viewer)s AND w.ended_at IS NULL
        AND (%(before_at)s::timestamptz IS NULL OR (p.posted_at, p.id) < (%(before_at)s, %(before_id)s))
        ORDER BY p.posted_at DESC, p.id DESC
        """,
        {
            "viewer": viewer.id,
            "before_at": None if ranked else before_at,
            "before_id": "" if ranked else before_id,
        },
    )
    candidates = [
        (post, author)
        for post, author in posts
        if viewer.can_view(post) and matches_feed(viewer, following, post, feed)
    ]
    scores: dict[str, int] = {}
    if ranked:
        scores = await recommendation_scores(
            db, viewer, [post for post, _ in candidates]
        )
        candidates.sort(key=lambda candidate: scores[candidate[0].id], reverse=True)
    items: list[FeedPostResponse] = []
    for post, author in candidates:
        score = scores.get(post.id)
        if (
            score is not None
            and score_cursor is not None
            and before_at is not None
            and not ranked_before(score, post, score_cursor, before_at, before_id)
        ):
            continue
        items.append(FeedPostResponse.of(post, author, score=score))
        if len(items) > 50:
            break
    return Page.of(items)
