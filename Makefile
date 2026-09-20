.PHONY: build test vet lint proto docker-up docker-down phase5-up phase5-down phase5-status

GOCMD ?= go

build:
	$(GOCMD) build ./...

test:
	$(GOCMD) test ./...

vet:
	$(GOCMD) vet ./...

proto:
	@echo "protoc not run automatically; install protoc and generate from proto/"

docker-up:
	docker compose -f deploy/local/docker-compose.yml up -d

docker-down:
	docker compose -f deploy/local/docker-compose.yml down -v

phase5-up:
	@powershell -NoProfile -ExecutionPolicy Bypass -Command "$ports = 50052,50053,8081,8082,8084; foreach ($port in $ports) { try { $cons = Get-NetTCPConnection -LocalPort $port -ErrorAction Stop; foreach ($c in $cons) { $pid = $c.OwnedProcess; if ($pid) { $proc = Get-Process -Id $pid -ErrorAction SilentlyContinue; if ($proc -and $proc.ProcessName -match 'go|cmd|node|java') { Stop-Process -Id $pid -Force -ErrorAction SilentlyContinue; Write-Host \"Stopped stale pid $pid on port $port\" } } } } catch { } }; Write-Host 'port cleanup complete'; Start-Process powershell -ArgumentList '-NoLogo','-NoExit','-Command','cd \"C:/Users/toluk/Desktop/osai\"; $$env:OSAI_CUSTOMER_GRPC_ADDR=\"localhost:50052\"; $$env:OSAI_SANDBOX_INSTITUTION_ID=\"inst_sandbox_local\"; $$env:OSAI_SANDBOX_CLIENT_ID=\"ck_sandbox_local\"; $$env:OSAI_SANDBOX_CLIENT_SECRET=\"secret_local_001\"; go run ./services/customer-kyb'; Start-Sleep -Seconds 1; Start-Process powershell -ArgumentList '-NoLogo','-NoExit','-Command','cd \"C:/Users/toluk/Desktop/osai\"; $$env:OSAI_QUOTE_GRPC_ADDR=\"localhost:50053\"; go run ./services/quote'; Start-Sleep -Seconds 1; Start-Process powershell -ArgumentList '-NoLogo','-NoExit','-Command','cd \"C:/Users/toluk/Desktop/osai\"; $$env:OSAI_CUSTOMER_GRPC_ADDR=\"localhost:50052\"; $$env:OSAI_QUOTE_GRPC_ADDR=\"localhost:50053\"; $$env:OSAI_API_GATEWAY_PORT=\"localhost:8082\"; go run ./services/api-gateway'"

phase5-down:
	@powershell -NoProfile -ExecutionPolicy Bypass -Command "$ports = 50052,50053,8081,8082,8084; foreach ($port in $ports) { try { $cons = Get-NetTCPConnection -LocalPort $port -ErrorAction Stop; foreach ($c in $cons) { $pid = $c.OwnedProcess; if ($pid) { $proc = Get-Process -Id $pid -ErrorAction SilentlyContinue; if ($proc -and $proc.ProcessName -match 'go|cmd|node|java') { Stop-Process -Id $pid -Force -ErrorAction SilentlyContinue; Write-Host \"Stopped pid $pid on port $port\" } } } } catch { } }; Write-Host 'phase5-down complete'"

phase5-status:
	@powershell -NoProfile -ExecutionPolicy Bypass -Command "$ports = 50052,50053,8081,8082,8084; foreach ($port in $ports) { try { $cons = Get-NetTCPConnection -LocalPort $port -ErrorAction Stop; foreach ($c in $cons) { $pid = $c.OwnedProcess; if ($pid) { $proc = Get-Process -Id $pid -ErrorAction SilentlyContinue; if ($proc) { Write-Host \"PORT $port PID $($proc.Id) NAME $($proc.ProcessName)\" } } } } catch { } }"