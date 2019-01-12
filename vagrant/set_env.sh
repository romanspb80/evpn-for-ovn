#!/bin/sh

unset $(grep -v '^#' ../app_settings.py | sed -E 's/(.*)=.*/\1/' | xargs)
export $(grep -v '^#' ../app_settings.py  | xargs -d '\n')