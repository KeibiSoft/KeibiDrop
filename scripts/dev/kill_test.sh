kill -9 $(lsof -i :26001 | cut -f2 -d ' ') && kill -9 $(lsof -i :26003 | cut -f2 -d ' ')
