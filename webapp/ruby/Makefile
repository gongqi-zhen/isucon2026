.PHONY: install run test

install:
	bundle config set --local deployment true
	bundle config set --local without test
	bundle install

run:
	bundle exec puma -C config/puma.rb

test:
	bundle config unset --local without
	bundle install
	bundle exec ruby -Itest -e 'Dir["test/*_test.rb"].each { |file| require File.expand_path(file) }'
