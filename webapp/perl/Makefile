.PHONY: install run test

# 依存モジュールを cpanfile.snapshot のとおりに local/ へ入れる
install:
	cpm install

run:
	PERL5LIB=$(CURDIR)/local/lib/perl5 perl local/bin/hypnotoad -f script/isureal

test:
	prove -Ilib -Ilocal/lib/perl5 -r t
