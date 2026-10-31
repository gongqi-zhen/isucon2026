"""API のレスポンス"""

from dataclasses import replace
from datetime import datetime, timedelta
from typing import Annotated, Any, Self

from pydantic import BaseModel, ConfigDict, Field, PlainSerializer
from pydantic.alias_generators import to_camel

from .models.comment import Comment
from .models.post import Location, Post
from .models.posting_window import PostingWindow
from .models.realmoji import RealMoji
from .models.user import User
from .timeutil import format_rfc3339

type Timestamp = Annotated[datetime, PlainSerializer(format_rfc3339, return_type=str)]


def _compact_number(value: float) -> float | int:
    # 整数値は 139.0 ではなく 139 と出す（Go の encoding/json と同じ）
    return int(value) if value.is_integer() else value


type Coordinate = Annotated[float, PlainSerializer(_compact_number)]


class Schema(BaseModel):
    model_config = ConfigDict(
        alias_generator=to_camel, validate_by_name=True, serialize_by_alias=True
    )


class Page[T](Schema):
    items: list[T]
    has_more: bool

    @classmethod
    def of(cls, items: list[T], limit: int = 50) -> Self:
        """limit + 1 件まで渡すと、超えた分から hasMore を決める"""
        return cls(items=items[:limit], has_more=len(items) > limit)


class SessionUserResponse(Schema):
    id: str
    username: str
    display_name: str
    bio: str
    region: str
    account_type: str
    status: str

    @classmethod
    def of(cls, user: User, **extra: Any) -> Self:
        return cls(
            id=user.id,
            username=user.username,
            display_name=user.display_name,
            bio=user.bio,
            region=user.region,
            account_type=user.account_type,
            status=user.status,
            **extra,
        )


class MeResponse(SessionUserResponse):
    profile_image_url: str | None

    @classmethod
    def of(cls, user: User, **extra: Any) -> Self:
        return super().of(user, profile_image_url=user.profile_image_url, **extra)


class UserResponse(Schema):
    id: str
    username: str
    display_name: str
    profile_image_url: str | None
    bio: str
    account_type: str

    @classmethod
    def of(cls, user: User, *, show_bio: bool = True) -> Self:
        """show_bio が False なら自己紹介を空にする"""
        if not show_bio:
            user = replace(user, bio="")
        return cls(
            id=user.id,
            username=user.username,
            display_name=user.display_name,
            profile_image_url=user.profile_image_url,
            bio=user.bio,
            account_type=user.account_type,
        )


class RegisterResponse(Schema):
    secret_key: str
    user: SessionUserResponse


class LoginResponse(Schema):
    token: str
    user: SessionUserResponse


def _is_none(value: object) -> bool:
    return value is None


class LocationResponse(Schema):
    kind: str
    latitude: Coordinate | None = Field(default=None, exclude_if=_is_none)
    longitude: Coordinate | None = Field(default=None, exclude_if=_is_none)
    locality: str | None = Field(default=None, exclude_if=_is_none)

    @classmethod
    def of(cls, location: Location) -> Self:
        return cls(
            kind=location.kind,
            latitude=location.latitude,
            longitude=location.longitude,
            locality=location.locality,
        )


class PostResponse(Schema):
    id: str
    author: UserResponse
    posting_window_id: str
    image_url: str
    caption: str
    visibility: str
    location: LocationResponse
    posted_at: Timestamp
    on_time: bool
    late_seconds: int

    @classmethod
    def of(cls, post: Post, author: User, **extra: Any) -> Self:
        on_time = post.posted_at <= post.on_time_deadline
        late_seconds = (
            0
            if on_time
            else (post.posted_at - post.on_time_deadline) // timedelta(seconds=1)
        )
        return cls(
            id=post.id,
            author=UserResponse.of(author),
            posting_window_id=post.posting_window_id,
            image_url=author.signed_image_url(post.image_id),
            caption=post.caption,
            visibility=post.visibility,
            location=LocationResponse.of(post.location),
            posted_at=post.posted_at,
            on_time=on_time,
            late_seconds=late_seconds,
            **extra,
        )


class FeedPostResponse(PostResponse):
    score: int | None = Field(default=None, exclude_if=_is_none)


class ExportPostResponse(PostResponse):
    image_path: str


class CommentResponse(Schema):
    id: str
    author: UserResponse
    body: str
    created_at: Timestamp

    @classmethod
    def of(cls, comment: Comment, author: User) -> Self:
        return cls(
            id=comment.id,
            author=UserResponse.of(author),
            body=comment.body,
            created_at=comment.created_at,
        )


class RealMojiResponse(Schema):
    id: str
    reactor: UserResponse
    reaction_type: str
    image_url: str
    created_at: Timestamp

    @classmethod
    def of(cls, reaction: RealMoji, reactor: User) -> Self:
        return cls(
            id=reaction.id,
            reactor=UserResponse.of(reactor),
            reaction_type=reaction.reaction_type,
            image_url=reactor.signed_image_url(reaction.image_id),
            created_at=reaction.created_at,
        )


class MemoryPostResponse(PostResponse):
    realmojis: list[RealMojiResponse] = Field(default_factory=list)
    comments: list[CommentResponse] = Field(default_factory=list)


class PostingWindowResponse(Schema):
    id: str
    region: str
    local_date: str
    starts_at: Timestamp
    on_time_deadline: Timestamp

    @classmethod
    def of(cls, window: PostingWindow) -> Self:
        return cls(
            id=window.id,
            region=window.region,
            local_date=window.local_date.isoformat(),
            starts_at=window.starts_at,
            on_time_deadline=window.on_time_deadline,
        )


class CurrentPostingWindowResponse(Schema):
    window: PostingWindowResponse | None = None
    has_posted: bool = False
    can_post: bool = False
    deletion_count: int = 0


class NotificationResponse(Schema):
    id: str
    posting_window_id: str
    region: str
    created_at: Timestamp


class FriendRequestResponse(Schema):
    relation_id: str
    status: str
    updated_at: Timestamp


class RelationshipResponse(Schema):
    relation_id: str
    user: UserResponse
    status: str
    updated_at: Timestamp


class AdminLoginResponse(Schema):
    token: str
    administrator_id: str


class InitializeResponse(Schema):
    language: str
    agent: str
    harness: str
    model: str
