package Isureal::Controller::Memory;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use Isureal::APIError;
use Isureal::Comment qw(select_comments_with_author comment_response);
use Isureal::Post qw(select_posts_with_author post_response);
use Isureal::RealMoji qw(select_current_real_mojis real_moji_response);
use Isureal::Util qw(parse_date placeholders);

sub by_month ($c) {
    my $month = $c->query_string('month');
    my $start = $month =~ /\A\d{4}-\d\d\z/ ? parse_date("$month-01") : undef;
    my $before_posted_at = $c->query_time('beforePostedAt');
    Isureal::APIError->throw(400, 'invalid_memory_period') unless $start;
    $c->render(json => $c->list_memories($start, $start->plus_months(1), $before_posted_at, $c->query_string('beforeId')));
}

sub by_date ($c) {
    my $before_posted_at = $c->query_time('beforePostedAt');
    my $start            = parse_date($c->param('date')) or Isureal::APIError->throw(400, 'invalid_memory_period');
    $c->render(json => $c->list_memories($start, $start->plus_days(1), $before_posted_at, $c->query_string('beforeId')));
}

sub list_memories ($c, $start, $end, $before_at, $before_id) {
    Isureal::APIError->throw(400, 'invalid_pagination') if defined $before_at xor $before_id ne '';
    my $db    = $c->db;
    my $posts = select_posts_with_author(
        $db, <<~'SQL',
          AND p.author_id = $1 AND w.local_date >= $2 AND w.local_date < $3
          AND ($4::timestamptz IS NULL OR (p.posted_at, p.id) < ($4, $5))
          ORDER BY p.posted_at DESC, p.id DESC LIMIT 51
        SQL
        $c->current_session->{user_id}, $start->strftime('%Y-%m-%d'), $end->strftime('%Y-%m-%d'),
        $before_at && $before_at->to_string, $before_id,
    );
    my @items = map { +{ post_response($_->{post}, $_->{author})->%*, realmojis => [], comments => [] } } @$posts;
    my $has_more = @items > 50;
    splice @items, 50 if $has_more;
    load_memory_interactions($db, \@items);
    return { items => \@items, hasMore => $has_more };
}

sub load_memory_interactions ($db, $items) {
    return unless @$items;
    my @post_ids = map { $_->{id} } @$items;
    my %item_by_post_id = map { $_->{id} => $_ } @$items;
    my $in = placeholders(scalar @post_ids);

    my $reactions = select_current_real_mojis($db, "AND r.post_id IN ($in) ORDER BY r.created_at DESC, r.id DESC", @post_ids);
    for my $reaction (@$reactions) {
        my $item = $item_by_post_id{$reaction->{real_moji}{post_id}};
        push $item->{realmojis}->@*, real_moji_response($reaction->{real_moji}, $reaction->{reactor})
            if $item->{realmojis}->@* < 50;
    }

    my $comments = select_comments_with_author($db, "AND c.post_id IN ($in) ORDER BY c.created_at, c.id", @post_ids);
    for my $comment (@$comments) {
        my $item = $item_by_post_id{$comment->{comment}{post_id}};
        push $item->{comments}->@*, comment_response($comment->{comment}, $comment->{author})
            if $item->{comments}->@* < 50;
    }
}

1;
