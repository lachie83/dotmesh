FROM ubuntu:artful
ENV SECURITY_UPDATES 2018-01-19
RUN apt-get -y update && apt-get -y install zfsutils-linux iproute kmod curl
# Merge kernel module search paths from CentOS and Ubuntu :-O
RUN echo 'search updates extra ubuntu built-in weak-updates' > /etc/depmod.d/ubuntu.conf
ADD require_zfs.sh /require_zfs.sh
COPY ./target/* /usr/local/bin/
