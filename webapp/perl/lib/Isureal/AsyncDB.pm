package Isureal::AsyncDB;
use v5.44;
use utf8;
use Mojo::Base -base;

use Mojo::Promise;

# 非同期クエリの同時実行数を limit 個に抑える。
# 非同期クエリは実行中のものごとに DB 接続を使うので、待たせないと接続数が増え続ける
has 'pg';
has limit   => 2;
has active  => 0;
has waiting => sub { [] };

sub query_p ($self, @args) {
    my $promise = Mojo::Promise->new;
    push $self->waiting->@*, [$promise, \@args];
    $self->_dispatch;
    return $promise;
}

sub _dispatch ($self) {
    while ($self->active < $self->limit && (my $job = shift $self->waiting->@*)) {
        my ($promise, $args) = @$job;
        my $db = eval { $self->pg->db };
        unless ($db) {
            $promise->reject($@);
            next;
        }
        $self->active($self->active + 1);
        $db->query_p(@$args)
            ->then(sub ($results) { $promise->resolve($results) }, sub ($err) { $promise->reject($err) })
            ->finally(sub {
                $self->active($self->active - 1);
                $self->_dispatch;
            });
    }
}

1;
