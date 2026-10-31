<?php

declare(strict_types=1);

namespace Isureal\Model;

/**
 * 投稿を見るユーザーと、その友達などの関係
 */
final readonly class ViewerContext
{
    /**
     * @param array<string, true> $friends
     * @param array<string, true> $friendsOfFriends
     * @param array<string, int> $mutualFriends
     * @param array<string, true> $blocked
     */
    public function __construct(
        public User $user,
        public bool $hasPostedToday,
        public array $friends,
        public array $friendsOfFriends,
        public array $mutualFriends,
        public array $blocked,
    ) {
    }

    public function isFriend(string $userId): bool
    {
        return isset($this->friends[$userId]);
    }

    public function isFriendOfFriend(string $userId): bool
    {
        return isset($this->friendsOfFriends[$userId]);
    }

    public function canView(Post $post): bool
    {
        if ($post->authorId === $this->user->id) {
            return true;
        }
        if ($post->windowEndedAt !== null || !$this->hasPostedToday || isset($this->blocked[$post->authorId])) {
            return false;
        }
        return match (Visibility::tryFrom($post->visibility)) {
            Visibility::Public => true,
            Visibility::Friends => $this->isFriend($post->authorId),
            Visibility::FriendsOfFriends => $this->isFriend($post->authorId) || $this->isFriendOfFriend($post->authorId),
            null => false,
        };
    }
}
