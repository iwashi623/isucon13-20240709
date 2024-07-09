now = $(shell date "+%Y%m%d%H%M%S")

.PHONY: bn
bn:
	make re
	cd ../bench && ./bench -all-addresses 127.0.0.11 -target 127.0.0.11:443 -tls -jia-service-url http://127.0.0.1:4999

# アプリ､nginx､mysqlの再起動
.PHONY: re
re:
	make arestart
	make nrestart
	make mrestart
	# ssh -A 172.31.0.169 "cd webapp && make mrestart"

.PHONY: build
build:
	cd ./go && go build -o isucondition

# アプリの再起動
.PHONY: arestart
arestart:
	make build
	sudo systemctl restart isucondition.go.service
	sudo systemctl status isucondition.go.service

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
	sudo cat /var/log/nginx/access.log | alp ltsv -m "/api/isu","/api/trend","/api/auth","/api/condition/[-0-9a-f]+","/assets/","/isu/[-0-9a-f]+/graph","/isu/[-0-9a-f]+/condition","/isu/[-0-9a-f]+" --sort=sum --reverse --filters 'Time > TimeAgo("10m")'

# mysqlのslowlogを見る
.PHONY: pt
pt:
	mv ~/pt.log ~/pt${now} 2>/dev/null || true
	sudo pt-query-digest /var/log/mysql/slow.log >> ~/pt.log
	cat ~/pt.log
