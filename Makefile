# agent-box — see README.md
.DEFAULT_GOAL := help

install: ## Install `agent` onto PATH, build the image, run doctor
	@./install.sh

test: ## Run unit tests (no container runtime needed)
	@bash test/unit.sh

doctor: ## Integration check: prove isolation holds (needs a runtime)
	@bash bin/agent doctor

lint: ## shellcheck the scripts
	@shellcheck -S warning bin/agent install.sh test/unit.sh

check: lint test ## What CI runs: lint + unit tests

help: ## List targets
	@grep -hE '^[a-z]+:.*##' $(MAKEFILE_LIST) | sed -E 's/:.*## / — /' | sort

.PHONY: install test doctor lint check help
