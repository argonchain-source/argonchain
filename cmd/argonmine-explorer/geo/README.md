# IP to country

`ip-country.csv.gz` is the IPv4 and IPv6 "geo-whois-asn-country" lists of
ip-location-db (https://github.com/sapics/ip-location-db, npm package
`@ip-location-db/geo-whois-asn-country` 2.3.2026061719), built from the
regional internet registries' delegation statistics. The data is licensed
under Creative Commons Attribution 4.0 by the Number Resource Organization
(https://www.nro.net); the credit is in docs/MINING.md ("The miner count on
the website"). Format: `first,last,country` per line.

The explorer uses it only to count miners per country (miners.go): the
country of the connection asking for work is kept, the IP address is not.
To update it, concatenate the two CSV files of a newer package version and
gzip them into this file.
