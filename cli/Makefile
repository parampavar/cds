.PHONY: clean

TARGET_DIR = ./dist
PKGS_COMMA_SEP = go list -f '{{ join .Deps "\n" }}{{"\n"}}{{.ImportPath}}' . | grep github.com/ovh/cds | grep -v vendor | tr '\n' ',' | sed 's/,$$//'
TEST_PKGS = $(shell go list ./...)
TEST_CMD = go test -tags skipkeychaintests -v -timeout 600s -coverprofile=profile.coverprofile

GO_COV_MERGE = ${GOPATH}/bin/gocovmerge
$(GO_COV_MERGE):
	go install github.com/wadey/gocovmerge@latest

GO_GOJUNIT = ${GOPATH}/bin/go-junit-report
$(GO_GOJUNIT):
	go install github.com/jstemmer/go-junit-report@latest

GO_COBERTURA = ${GOPATH}/bin/gocover-cobertura
$(GO_COBERTURA):
	go install github.com/richardlt/gocover-cobertura@latest

GO_XUTOOLS = $(GOPATH)/bin/xutools
$(GO_XUTOOLS):
	go install github.com/richardlt/xutools@latest

$(TARGET_DIR):
	@mkdir -p $(TARGET_DIR)

clean:
	@rm -rf $(TARGET_DIR)
	@for TST in `find ./ -name "tests.log"`; do \
		rm $$TST; \
	done;
	@for profile in `find ./ -name "*.coverprofile"`; do \
		rm $$profile; \
	done;

test: clean
	$(info $(TEST_PKGS))
	@for PKG in $(TEST_PKGS); do \
		echo "Running tests in package $$PKG"; \
		cd ${GOPATH}/src/$$PKG; \
		TEMP=`$(PKGS_COMMA_SEP)`; \
    if [ ! -z "$$TEMP" ]; then \
		  $(TEST_CMD) -coverpkg $$TEMP > ${GOPATH}/src/$$PKG/tests.log; \
    else \
		  $(TEST_CMD) > ${GOPATH}/src/$$PKG/tests.log; \
    fi; \
	done
	$(MAKE) test-coverage
	$(MAKE) test-xunit-report

test-coverage: $(GO_COV_MERGE) $(TARGET_DIR) $(GO_COBERTURA)
	@echo "Generating unit tests coverage..."
	@$(GO_COV_MERGE) `find ./ -name "*.coverprofile"` > $(TARGET_DIR)/cover.out
	@$(GO_COBERTURA) < $(TARGET_DIR)/cover.out > $(TARGET_DIR)/coverage.xml
	@go tool cover -html=$(TARGET_DIR)/cover.out -o=$(TARGET_DIR)/cover.html

#  <testsuite name="github.com/ovh/cds/engine/api" tests="133" errors="0" failures="2" skip="0">
test-xunit-report: $(GO_GOJUNIT) $(GO_XUTOOLS) $(TARGET_DIR)
	@echo "Generating xUnit Report..."
	@for TST in `find . -name "tests.log"`; do \
		if [ -s $$TST ]; then \
			FAILED=`grep -E '(FAIL)+\s([a-z\.\/]*)\s\[build failed\]' $$TST | wc -l`; \
			if [ $$FAILED -gt 0 ]; then \
				echo "Build Failed \t\t\t($$TST)"; \
				echo "Build Failed \t\t\t($$TST)" >>  $(TARGET_DIR)/fail; \
			else \
				NO_TESTS=`grep -E '\?+\s+([a-z\.\/]*)\s\[no test files\]' $$TST | wc -l`; \
				if [ $$NO_TESTS -gt 0 ]; then \
					echo "No tests found \t\t\t($$TST)"; \
				else \
          if [ "$$TST" = "./tests.log" ]; then \
            PACKAGE=cli; \
					else \
            PACKAGE=cli_`echo $$TST | sed 's|./||' | sed 's|/|_|g' | sed 's|_tests.log||'`; \
          fi; \
          XUNIT_OUT=$(TARGET_DIR)/$$PACKAGE.xml; \
          echo "Generating xUnit report from \t$$TST.xml to $$XUNIT_OUT"; \
					cat $$TST | $(GO_GOJUNIT) > $$XUNIT_OUT; \
          TESTS_LOG_OUT=$(TARGET_DIR)/$$PACKAGE.log; \
          echo "Copy tests log from \t$$TST.xml to $$TESTS_LOG_OUT"; \
          cp $$TST $$TESTS_LOG_OUT; \
				fi; \
			fi; \
		else \
			echo "Ignoring empty file \t\t$$TST"; \
		fi; \
	done; \
	$(GO_XUTOOLS) pretty --show-failures ./dist/*.xml > $(TARGET_DIR)/report; \
  $(GO_XUTOOLS) sort-duration ./dist/*.xml > $(TARGET_DIR)/duration; \
	if [ -e $(TARGET_DIR)/report ]; then \
    echo "Report:"; \
		cat $(TARGET_DIR)/report; \
	fi; \
  if [ -e $(TARGET_DIR)/duration ]; then \
    echo "Max duration:"; \
		cat $(TARGET_DIR)/duration; \
	fi; \
	if [ -e $(TARGET_DIR)/fail ]; then \
		echo "#########################"; \
		echo "ERROR: Test compilation failure"; \
		cat $(TARGET_DIR)/fail; \
		exit 1; \
	fi;
