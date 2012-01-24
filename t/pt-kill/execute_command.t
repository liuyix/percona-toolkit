#!/usr/bin/env perl

BEGIN {
   die "The PERCONA_TOOLKIT_BRANCH environment variable is not set.\n"
      unless $ENV{PERCONA_TOOLKIT_BRANCH} && -d $ENV{PERCONA_TOOLKIT_BRANCH};
   unshift @INC, "$ENV{PERCONA_TOOLKIT_BRANCH}/lib";
};

use strict;
use warnings FATAL => 'all';
use English qw(-no_match_vars);
use Test::More tests => 8;

use PerconaTest;
use Sandbox;
require "$trunk/bin/pt-kill";

my $dp = new DSNParser(opts=>$dsn_opts);
my $sb = new Sandbox(basedir => '/tmp', DSNParser => $dp);
my $master_dbh = $sb->get_dbh_for('master');

my $output;
my $cnf ='/tmp/12345/my.sandbox.cnf';
my $cmd = "$trunk/bin/pt-kill -F $cnf -h 127.1";
my $out = "/tmp/mk-kill-test.txt";

# #############################################################################
# Test --execute-command action.
# #############################################################################
diag(`rm $out 2>/dev/null`);

$output = `$cmd $trunk/t/lib/samples/pl/recset001.txt --match-command Query --execute-command 'echo hello > $out'`;
is(
   $output, 
   '',
   'No output without --print'
);

chomp($output = `cat $out`),
is(
   $output,
   'hello',
   '--execute-command'
);

diag(`rm $out`);

SKIP: {
   skip 'Cannot connect to sandbox master', 2 unless $master_dbh;

   system "/tmp/12345/use -e 'select sleep(2)' >/dev/null 2>&1 &";

   $output = `$cmd --match-info 'select sleep' --run-time 2 --interval 1 --print --execute-command 'echo batty > $out'`;

   like(
      $output,
      qr/KILL .+ select sleep\(2\)/,
      '--print with --execute-command'
   );

   chomp($output = `cat $out`);
   is(
      $output,
      'batty',
      '--execute-command (online)'
   );

   # Let our select sleep(2) go away before other tests are ran.
   sleep 1;
   diag(`rm $out`);

   # Don't make zombies (https://bugs.launchpad.net/percona-toolkit/+bug/919819)
   system "/tmp/12345/use -e 'select sleep(2)' >/dev/null 2>&1 &";

   my $sentinel = "/tmp/pt-kill-test.$PID.stop";
   my $pid_file = "/tmp/pt-kill-test.$PID.pid";
   my $log_file = "/tmp/pt-kill-test.$PID.log";
   diag(`rm $sentinel 2>/dev/null`);
   diag(`rm $pid_file 2>/dev/null`);
   diag(`rm $log_file 2>/dev/null`);

   `$cmd --daemonize --match-info 'select sleep' --interval 1 --print --execute-command 'echo zombie > $out' --verbose --pid $pid_file --log $log_file --sentinel $sentinel`;
   sleep 1;
   $output = `grep Executed $log_file`;
   like(
      $output,
      qr/Executed echo zombie/,
      "Executed zombie command"
   );

   $output = `ps x | grep Z | grep -v grep`;
   is(
      $output,
      "",
      "No zombies"
   );

   diag(`touch $sentinel`);
   sleep 1;
   ok(
      !-f $pid_file,
      "pt-kill stopped"
   );
   $output = `ps x | grep Z | grep -v grep`;
   is(
      $output,
      "",
      "No zombies"
   );

   diag(`rm $sentinel 2>/dev/null`);
   diag(`rm $pid_file 2>/dev/null`);
   diag(`rm $log_file 2>/dev/null`);
}

# #############################################################################
# Done.
# #############################################################################
diag(`rm $out 2>/dev/null`);
$sb->wipe_clean($master_dbh) if $master_dbh;
exit;
