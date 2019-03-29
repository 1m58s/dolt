#!/usr/bin/env bats

setup() {
    export PATH=$PATH:~/go/bin
    export NOMS_VERSION_NEXT=1
    cd $BATS_TMPDIR
    mkdir dolt-repo
    cd dolt-repo
    dolt init
    dolt table create -s=$BATS_TEST_DIRNAME/helper/1pk5col.schema test1
    dolt table create -s=$BATS_TEST_DIRNAME/helper/1pk5col.schema test2
}

teardown() {
    rm -rf $BATS_TMPDIR/dolt-repo
}

@test "examine a multi table repo" {
      run dolt ls
      [ "$status" -eq 0 ]
      [[ "$output" =~ "test1" ]] || false
      [[ "$output" =~ "test2" ]] || false
      [ "${#lines[@]}" -eq 3 ]
      run dolt table schema
      [ "$status" -eq 0 ]
      [[ "$output" =~ "test1 @ working" ]] || false
      [[ "$output" =~ "test2 @ working" ]] || false
      run dolt status 
      [ "$status" -eq 0 ]
      [[ "$output" =~ "test1" ]] || false
      [[ "$output" =~ "test2" ]] || false
}

@test "modify both tables, commit only one" {
    dolt table put-row test1 pk:0 c1:1 c2:2 c3:3 c4:4 c5:5
    dolt table put-row test2 pk:0 c1:1 c2:2 c3:3 c4:4 c5:5
    dolt add test1
    run dolt status
    [[ "$output" =~ "Changes to be committed" ]] || false
    [[ "$output" =~ "Untracked files" ]] || false
    run dolt commit -m "added one table"
    run dolt status
    [[ ! "$output" =~ "Changes to be committed" ]] || false
    [[ "$output" =~ "Untracked files" ]] || false
    run dolt diff
    [[ "$output" =~ "test2" ]] || false
    run dolt checkout test2
    [ "$output" = "" ]
    run dolt status
    [[ "$output" =~ "nothing to commit" ]] || false
    run dolt ls
    [[ "$output" =~ "test1" ]] || false
    [[ ! "$output" =~ "test2" ]] || false
    [ "${#lines[@]}" -eq 2 ]
}

@test "dolt add --all and dolt add . adds all changes" {
    dolt table put-row test1 pk:0 c1:1 c2:2 c3:3 c4:4 c5:5
    dolt table put-row test2 pk:0 c1:1 c2:2 c3:3 c4:4 c5:5
    dolt add --all
    run dolt status
    [[ "$output" =~ "Changes to be committed" ]] || false
    [[ ! "$output" =~ "Untracked files" ]] || false
    run dolt reset test1 test2
    [ "$status" -eq 0 ]
    [ "$output" = "" ]
    run dolt status 
    [[ ! "$output" =~ "Changes to be committed" ]] || false
    [[ "$output" =~ "Untracked files" ]] || false
    run dolt add .
    [ "$status" -eq 0 ]
    [ "$output" = "" ]
    run dolt status
    [[ "$output" =~ "Changes to be committed" ]] || false
    [[ ! "$output" =~ "Untracked files" ]] || false
}