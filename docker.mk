_COMPOSE=docker compose -f docker-compose.yml --project-name ${NAMESPACE}

docker-build: ## Build stress test
	${_COMPOSE} build floxy-stress-tester

docker-up: ## Up the environment in docker compose
	${_COMPOSE} up -d

docker-down: ## Down the environment in docker compose
	${_COMPOSE} down --remove-orphans

docker-clean: ## Down the environment in docker compose with images cleanup
	${_COMPOSE} down --remove-orphans -v --rmi all

docker-restart: docker-down docker-up ## Restart the environment in docker compose
