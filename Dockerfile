FROM alpine

ADD main main

EXPOSE 8080

CMD ./main
