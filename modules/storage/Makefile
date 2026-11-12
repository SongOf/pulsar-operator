.PHONY: demo test inspect render image
demo:
	python3 -m diskfleet.api
test:
	python3 -m unittest discover -s tests -v
inspect:
	python3 scripts/inspect-disks.py
render:
	kubectl kustomize deploy
image:
	docker build -t diskfleet:dev .
