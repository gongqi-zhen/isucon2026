<?php

declare(strict_types=1);

namespace Isureal\Model;

enum Visibility: string
{
    case Friends = 'friends';
    case FriendsOfFriends = 'friends_of_friends';
    case Public = 'public';
}
