from collections import Counter
from collections.abc import Mapping
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any

from ..db import Connection, Params, fetch_all, fetch_one, fetch_value, from_row
from .relationship import get_blocked_user_ids, get_friend_ids
from .user import User, get_user, user_columns, valid_user_text

VISIBILITIES = frozenset({"friends", "friends_of_friends", "public"})


@dataclass(slots=True, frozen=True)
class Location:
    kind: str
    latitude: float | None = None
    longitude: float | None = None
    locality: str | None = None

    def is_valid(self) -> bool:
        match self.kind:
            case "none":
                return (self.latitude, self.longitude, self.locality) == (
                    None,
                    None,
                    None,
                )
            case "exact":
                return (
                    self.latitude is not None
                    and self.longitude is not None
                    and self.locality is None
                    and -90 <= self.latitude <= 90
                    and -180 <= self.longitude <= 180
                )
            case "approximate":
                return (
                    self.latitude is None
                    and self.longitude is None
                    and self.locality is not None
                    and valid_user_text(self.locality, 1, 100)
                )
            case _:
                return False


POST_COLUMNS = """
  p.id, p.author_id, p.posting_window_id, p.image_id, p.caption, p.visibility,
  p.location_kind, p.latitude, p.longitude, p.locality, p.posted_at,
  w.on_time_deadline, w.ended_at AS window_ended_at
"""


@dataclass(slots=True)
class Post:
    id: str
    author_id: str
    posting_window_id: str
    image_id: str
    caption: str
    visibility: str
    location: Location
    posted_at: datetime
    on_time_deadline: datetime
    window_ended_at: datetime | None

    @classmethod
    def from_row(cls, row: Mapping[str, Any]) -> Post:
        return cls(
            id=row["id"],
            author_id=row["author_id"],
            posting_window_id=row["posting_window_id"],
            image_id=row["image_id"],
            caption=row["caption"],
            visibility=row["visibility"],
            location=Location(
                kind=row["location_kind"],
                latitude=row["latitude"],
                longitude=row["longitude"],
                locality=row["locality"],
            ),
            posted_at=row["posted_at"],
            on_time_deadline=row["on_time_deadline"],
            window_ended_at=row["window_ended_at"],
        )


async def get_post(db: Connection, post_id: str) -> Post | None:
    row = await fetch_one(
        db,
        f"""
        SELECT {POST_COLUMNS}
        FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
        WHERE p.id = %(post_id)s AND p.deleted_at IS NULL
        """,
        {"post_id": post_id},
    )
    return Post.from_row(row) if row else None


async def select_posts_with_author(
    db: Connection, conditions: str, params: Params
) -> list[tuple[Post, User]]:
    rows = await fetch_all(
        db,
        f"""
        SELECT {POST_COLUMNS}, {user_columns("u", "author")}
        FROM posts p
        JOIN posting_windows w ON w.id = p.posting_window_id
        JOIN users u ON u.id = p.author_id AND u.status = 'active'
        WHERE p.deleted_at IS NULL {conditions}
        """,
        params,
    )
    return [(Post.from_row(row), from_row(User, row, "author.")) for row in rows]


async def get_post_with_author(
    db: Connection, post_id: str
) -> tuple[Post, User] | None:
    posts = await select_posts_with_author(
        db, "AND p.id = %(post_id)s", {"post_id": post_id}
    )
    return posts[0] if posts else None


@dataclass(slots=True)
class Viewer:
    user: User
    has_posted_today: bool
    blocked: set[str]
    friends: set[str] = field(default_factory=set)
    friends_of_friends: set[str] = field(default_factory=set)
    mutual_friends: Counter[str] = field(default_factory=Counter)

    @property
    def id(self) -> str:
        return self.user.id

    def can_view(self, post: Post) -> bool:
        if post.author_id == self.id:
            return True
        if (
            post.window_ended_at is not None
            or not self.has_posted_today
            or post.author_id in self.blocked
        ):
            return False
        match post.visibility:
            case "public":
                return True
            case "friends":
                return post.author_id in self.friends
            case "friends_of_friends":
                return (
                    post.author_id in self.friends
                    or post.author_id in self.friends_of_friends
                )
            case _:
                return False


async def load_viewer(db: Connection, viewer_id: str) -> Viewer | None:
    user = await get_user(db, viewer_id)
    if user is None:
        return None
    has_posted_today: bool = await fetch_value(
        db,
        """
        SELECT EXISTS(
          SELECT 1 FROM posts p
          JOIN posting_windows posted ON posted.id = p.posting_window_id
          JOIN posting_windows current ON current.region = %(region)s AND current.ended_at IS NULL
                                      AND current.local_date = posted.local_date
          WHERE p.author_id = %(user_id)s AND p.deleted_at IS NULL
        )
        """,
        {"user_id": user.id, "region": user.region},
    )
    viewer = Viewer(
        user=user,
        has_posted_today=has_posted_today,
        blocked=await get_blocked_user_ids(db, user.id),
    )
    viewer.friends = set(await get_friend_ids(db, user.id))
    if not viewer.friends:
        return viewer
    friendships = await fetch_all(
        db,
        """
        SELECT user_low_id, user_high_id FROM friendships
        WHERE user_low_id = ANY(%(ids)s) OR user_high_id = ANY(%(ids)s)
        """,
        {"ids": list(viewer.friends)},
    )
    for friendship in friendships:
        low, high = friendship["user_low_id"], friendship["user_high_id"]
        if low in viewer.friends:
            viewer.friends_of_friends.add(high)
            if high != viewer.id:
                viewer.mutual_friends[high] += 1
        if high in viewer.friends:
            viewer.friends_of_friends.add(low)
            if low != viewer.id:
                viewer.mutual_friends[low] += 1
    return viewer


async def can_view_post(db: Connection, viewer_id: str, post_id: str) -> bool:
    post = await get_post_with_author(db, post_id)
    if post is None:
        return False
    viewer = await load_viewer(db, viewer_id)
    return viewer is not None and viewer.can_view(post[0])
