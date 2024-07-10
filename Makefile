now = $(shell date "+%Y%m%d%H%M%S")
DARWIN_TARGET_ENV=GOOS=darwin GOARCH=arm64
LINUX_TARGET_ENV=GOOS=linux GOARCH=amd64

BUILD=go build

DOCKER_BUILD=sudo docker build
DOCKER_BUILD_OPTS=--no-cache

DOCKER_RMI=sudo docker rmi -f

DESTDIR=.
TAG=isupipe:latest

.PHONY: bn
bn:
	make re
	cd ../bench && ./bench -all-addresses 127.0.0.11 -target 127.0.0.11:443 -tls -jia-service-url http://127.0.0.1:4999

# アプリ､nginx､mysqlの再起動
.PHONY: re
re:
	make arestart
	make nrestart
	# make mrestart
	ssh -A 192.168.0.12 "cd webapp && make mrestart"
	ssh -A 192.168.0.13 "cd webapp && make mrestart"

.PHONY: build
build:
	cd go && CGO_ENABLED=0 $(LINUX_TARGET_ENV)  $(BUILD) -o $(DESTDIR)/isupipe -ldflags "-s -w"

# アプリの再起動
.PHONY: arestart
arestart:
	make build
	sudo systemctl restart isupipe-go.service
	sudo systemctl status isupipe-go.service

# nginxの再起動
.PHONY: nrestart
nrestart:
	sudo rm /var/log/nginx/access.log
	sudo systemctl reload nginx
	sudo systemctl status nginx

# mysqlの再起動
.PHONY: mrestart
mrestart:
	sudo rm /var/log/mysql/slow.log
	sudo mysqladmin flush-logs -pisupipe
	sudo systemctl restart mysql
	sudo systemctl status mysql
	echo "set global slow_query_log = 1;" | sudo mysql -pisupipe
	echo "set global slow_query_log_file = '/var/log/mysql/slow.log';" | sudo mysql -pisupipe
	echo "set global long_query_time = 0;" | sudo mysql -pisupipe

# アプリのログを見る
.PHONY: nalp
nalp:
	sudo cat /var/log/nginx/access.log | alp ltsv -m "/api/user/[-0-9a-zA-Z]+","/api/livestream/[-0-9a-zA-Z]+/livecomment","/api/livestream/[-0-9a-zA-Z]+/livecomment/[-0-9]+/report","/api/livestream/[-0-9a-zA-Z]+/livecomment","/api/livestream/[-0-9a-zA-Z]+/reaction","/api/livestream/[-0-9a-zA-Z]+/moderate","/api/livestream/[-0-9a-zA-Z]+/statistics","/api/livestream/[-0-9a-zA-Z]+/livecomment","/api/livestream/[-0-9a-zA-Z]+/reaction","/api/livestream/[-0-9a-zA-Z]+/report","/api/livestream/[-0-9a-zA-Z]+/ngwords","/api/livestream/[-0-9a-zA-Z]+/enter","/api/livestream/[-0-9a-zA-Z]+/exit","/api/user/[-0-9a-zA-Z]+/theme"  --sort=sum --reverse --filters 'Time > TimeAgo("30m")'

# mysqlのslowlogを見る
.PHONY: pt
pt:
	mv ~/pt.log ~/pt${now} 2>/dev/null || true
	sudo pt-query-digest /var/log/mysql/slow.log >> ~/pt.log
	cat ~/pt.log
