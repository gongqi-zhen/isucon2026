package Isureal;
use v5.44;
use utf8;
use Mojo::Base 'Mojolicious';

use Mojo::Pg;
use Isureal::APIError;
use Isureal::ActionLog;
use Isureal::AsyncDB;
use Isureal::Config;
use Isureal::IPBan;
use Isureal::User qw(valid_region);

sub startup ($self) {
    my $config = Isureal::Config->from_env;
    die "invalid server region: $config->{region}\n" unless valid_region($config->{region});
    $self->config(isureal => $config);

    $self->config(hypnotoad => {
        listen  => ["http://*:$config->{port}"],
        workers => $config->{workers},
        # 通知は接続を長く保持するので、1 worker で多数の接続を受け付ける
        clients            => 1000,
        inactivity_timeout => 60,
        # 接続数で worker を入れ替えない
        accepts => 0,
        # 初期化のように長くかかる同期処理の途中で worker が止められないようにする
        heartbeat_timeout => 120,
        pid_file          => $self->home->child('hypnotoad.pid')->to_string,
    });
    $self->max_request_size(64 * 1024 * 1024);
    $self->renderer->compress(0);
    $self->types->type(json => 'application/json');
    $self->secrets(['isureal']);
    $self->controller_class('Isureal::Controller');
    $self->routes->namespaces(['Isureal::Controller']);

    my $pg = Mojo::Pg->new
        ->dsn($config->database_dsn)
        ->username($config->{db_user})
        ->password($config->{db_password})
        ->max_connections(1 + $config->{async_db_connections});
    $pg->on(connection => sub ($pg, $dbh) { $dbh->do(q{SET TIME ZONE 'UTC'}) });
    my $async_db = Isureal::AsyncDB->new(pg => $pg, limit => $config->{async_db_connections});
    my $ip_bans  = Isureal::IPBan->new;
    STDOUT->autoflush(1);
    my $action_log = Isureal::ActionLog->new(min_level => $config->{log_level}, output => \*STDOUT);

    $self->helper(pg         => sub ($c) { $pg });
    $self->helper(async_db   => sub ($c) { $async_db });
    $self->helper(db         => sub ($c) { $c->stash->{'isureal.db'} //= $pg->db });
    $self->helper(region     => sub ($c) { $config->{region} });
    $self->helper(ip_bans    => sub ($c) { $ip_bans });
    $self->helper(action_log => sub ($c) { $action_log });

    $self->hook(around_action => sub ($next, $c, $action, $last) {
        try {
            return $next->();
        }
        catch ($e) {
            $c->render_error($e);
            return undef;
        }
    });
    $self->hook(after_dispatch => sub ($c) { $c->action_log->record($c) });

    $self->setup_routes;
}

sub setup_routes ($self) {
    my $api = $self->routes->under('/api')->to('IPBan#check');

    $api->post('/initialize')->to('initialize#initialize')->name('initialize');
    $api->post('/auth/register')->to('account#register')->name('register');
    $api->post('/auth/login')->to('account#login')->name('login');
    $api->get('/images/<#imageId>')->to('image#show')->name('get_image');
    $api->post('/admin/auth/login')->to('admin#login')->name('admin_login');

    my $session = $api->under('/')->to('session#authenticate');
    $session->post('/auth/logout')->to('account#logout')->name('logout');
    $session->patch('/me')->to('account#update_me')->name('update_me');
    $session->put('/me/profile-image')->to('profile_image#update')->name('put_profile_image');
    $session->delete('/me/profile-image')->to('profile_image#remove')->name('delete_profile_image');

    my $regional = $session->under('/')->to('session#check_region');
    $regional->get('/users')->to('user#search')->name('search_users');
    $regional->get('/users/<#userId>')->to('user#show')->name('get_user');
    $regional->get('/me/export')->to('export#export_posts')->name('export_posts');
    $regional->get('/me')->to('account#me')->name('get_me');
    $regional->post('/me/account-deletion')->to('account#delete_account')->name('delete_account');
    $regional->post('/posts')->to('post#create')->name('create_post');
    $regional->get('/posting-window/current')->to('posting_window#current')->name('get_current_posting_window');
    $regional->get('/posts/me/current')->to('post#my_current')->name('get_my_current_post');
    $regional->get('/posts/<#postId>')->to('post#show')->name('get_post');
    $regional->patch('/posts/<#postId>')->to('post#update')->name('update_post');
    $regional->delete('/posts/<#postId>')->to('post#remove')->name('delete_post');
    $regional->get('/feeds/<#feed>')->to('feed#show')->name('get_feed');
    $regional->get('/memories')->to('memory#by_month')->name('get_memories');
    $regional->get('/memories/<#date>')->to('memory#by_date')->name('get_memories_by_date');
    $regional->post('/posts/<#postId>/comments')->to('comment#create')->name('create_comment');
    $regional->get('/posts/<#postId>/comments')->to('comment#list')->name('get_comments');
    $regional->delete('/comments/<#commentId>')->to('comment#remove')->name('delete_comment');
    $regional->post('/posts/<#postId>/realmojis')->to('real_moji#replace')->name('replace_real_moji');
    $regional->get('/posts/<#postId>/realmojis')->to('real_moji#list')->name('get_real_mojis');
    $regional->delete('/realmojis/<#realMojiId>')->to('real_moji#remove')->name('delete_real_moji');
    $regional->get('/notifications')->to('notification#stream')->name('get_notifications');
    $regional->post('/friend-requests')->to('friend_request#create')->name('create_friend_request');
    $regional->post('/friend-requests/<#requestId>/<#requestAction>')->to('friend_request#transition')->name('transition_friend_request');
    $regional->get('/relationships/<#kind>')->to('relationship#list')->name('get_relationships');
    $regional->delete('/friendships/<#userId>')->to('relationship#delete_friendship')->name('delete_friendship');
    $regional->put('/follows/<#userId>')->to('relationship#follow')->name('follow');
    $regional->delete('/follows/<#userId>')->to('relationship#unfollow')->name('unfollow');
    $regional->put('/blocks/<#userId>')->to('relationship#block')->name('block');
    $regional->delete('/blocks/<#userId>')->to('relationship#unblock')->name('unblock');

    my $admin = $api->under('/')->to('admin_session#authenticate');
    $admin->post('/admin/auth/logout')->to('admin#logout')->name('admin_logout');
    $admin->put('/admin/users/<#userId>/certification')->to('admin#certify_user')->name('certify_user');
    $admin->delete('/admin/users/<#userId>/certification')->to('admin#delete_user_certification')->name('delete_user_certification');
    $admin->post('/admin/posting-window')->to('admin#open_posting_window')->name('open_posting_window');

    $api->any('/*whatever' => {whatever => ''})->to('initialize#not_found');
}

1;
